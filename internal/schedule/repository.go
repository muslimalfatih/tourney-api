package schedule

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/muslimalfatih/laga-api/internal/audit"
)

var (
	ErrNotFound  = errors.New("not found")
	ErrForbidden = errors.New("not permitted")
	// ErrStateConflict: a concurrent write won the race and the DB exclusion
	// constraint (or a mid-flight row change) rejected ours — retryable 409.
	ErrStateConflict = errors.New("schedule changed concurrently")
)

// restBuffer is the minimum turnaround a pair should get between matches.
// Violations warn rather than block, and the organizer can override.
// ponytail: fixed rule; becomes per-tournament config if organizers ever ask.
const restBuffer = 30 * time.Minute

// Conflict describes one existing slot that clashes with a requested write.
type Conflict struct {
	Type       string     `json:"type"` // court_overlap | participant_overlap | rest_buffer
	SlotID     uuid.UUID  `json:"slot_id"`
	CourtID    uuid.UUID  `json:"court_id"`
	CourtName  string     `json:"court_name"`
	MatchID    *uuid.UUID `json:"match_id,omitempty"`
	MatchLabel *string    `json:"match_label,omitempty"`
	StartsAt   time.Time  `json:"starts_at"`
	EndsAt     time.Time  `json:"ends_at"`
}

// ConflictError carries structured conflicts out of a rejected slot write.
// Hard conflicts (court/participant overlap) always block; Rest entries are
// warnings that block only until the organizer resubmits with an explicit
// override_rest_buffer.
type ConflictError struct {
	Hard []Conflict
	Rest []Conflict
}

func (e *ConflictError) Error() string { return "schedule conflict" }

type Court struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	SortOrder int       `json:"sort_order"`
}

// Slot is a scheduled match on a court at a time. MatchLabel is a human summary
// of the assigned match (participant names) for display.
//
// EventName/MatchStatus are nullable together with MatchID/MatchLabel — a slot
// can be "held" (a blocked court with no match assigned yet), which has none
// of these. EventName is the division's public-facing display name, not the
// raw event.name: a public schedule spanning several divisions needs the same
// label a visitor already sees elsewhere on the tournament (public_display_name),
// not internal naming.
//
// No RoundName: round labels ("Quarterfinal", "Round of 16") are computed
// algorithmically from the match progression graph at bracket-read time
// (draw.Service.GetBracket / knockoutRounds hop-counting from next_match_id),
// never persisted to matches.round_id or the rounds table — confirmed live,
// every match's round_id came back NULL. Surfacing it here would need either
// replicating that graph walk per-slot or changing what draw generation
// persists; both are bigger than this endpoint.
type Slot struct {
	ID          uuid.UUID  `json:"id"`
	CourtID     uuid.UUID  `json:"court_id"`
	CourtName   string     `json:"court_name"`
	MatchID     *uuid.UUID `json:"match_id"`
	MatchLabel  *string    `json:"match_label"`
	EventID     *uuid.UUID `json:"event_id"`
	EventName   *string    `json:"event_name"`
	MatchStatus *string    `json:"match_status"`
	StartsAt    time.Time  `json:"starts_at"`
	EndsAt      time.Time  `json:"ends_at"`
}

type Repository struct {
	pool  *pgxpool.Pool
	audit *audit.Service
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool, audit: audit.NewService(pool)}
}

// TournamentSlugPublished returns the tournament's slug and whether it is
// published — the SSE broadcast gate (draft schedules never broadcast).
func (r *Repository) TournamentSlugPublished(ctx context.Context, tournamentID uuid.UUID) (string, bool, error) {
	var slug string
	var published bool
	err := r.pool.QueryRow(ctx,
		`SELECT slug, status = 'published' FROM tournaments WHERE id = $1`, tournamentID).
		Scan(&slug, &published)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, ErrNotFound
	}
	return slug, published, err
}

// TournamentOrg returns the owning org of a tournament (for authz).
func (r *Repository) TournamentOrg(ctx context.Context, tournamentID uuid.UUID) (uuid.UUID, error) {
	var orgID uuid.UUID
	err := r.pool.QueryRow(ctx, `SELECT org_id FROM tournaments WHERE id = $1`, tournamentID).Scan(&orgID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrNotFound
	}
	return orgID, err
}

// defaultVenue returns (creating if needed) the tournament's default venue id.
// Organizers manage courts directly; venues are an implementation detail.
func (r *Repository) defaultVenue(ctx context.Context, tx pgx.Tx, tournamentID uuid.UUID) (uuid.UUID, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx,
		`SELECT id FROM venues WHERE tournament_id = $1 ORDER BY id LIMIT 1`, tournamentID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.QueryRow(ctx,
			`INSERT INTO venues (tournament_id, name) VALUES ($1, 'Main Venue') RETURNING id`,
			tournamentID).Scan(&id); err != nil {
			return uuid.Nil, err
		}
		return id, nil
	}
	return id, err
}

func (r *Repository) ListCourts(ctx context.Context, tournamentID uuid.UUID) ([]Court, error) {
	const q = `
		SELECT c.id, c.name, c.sort_order
		FROM courts c JOIN venues v ON v.id = c.venue_id
		WHERE v.tournament_id = $1
		ORDER BY c.sort_order, c.name`
	rows, err := r.pool.Query(ctx, q, tournamentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Court{}
	for rows.Next() {
		var c Court
		if err := rows.Scan(&c.ID, &c.Name, &c.SortOrder); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r *Repository) CreateCourt(ctx context.Context, tournamentID uuid.UUID, name string, sortOrder int) (*Court, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	venueID, err := r.defaultVenue(ctx, tx, tournamentID)
	if err != nil {
		return nil, err
	}
	var c Court
	if err := tx.QueryRow(ctx,
		`INSERT INTO courts (venue_id, name, sort_order) VALUES ($1, $2, $3)
		 RETURNING id, name, sort_order`, venueID, name, sortOrder).
		Scan(&c.ID, &c.Name, &c.SortOrder); err != nil {
		return nil, err
	}
	return &c, tx.Commit(ctx)
}

// ListSlots returns every scheduled slot for the tournament — the organizer's
// own view, so a private/draft division's slots are included on purpose.
func (r *Repository) ListSlots(ctx context.Context, tournamentID uuid.UUID) ([]Slot, error) {
	return r.listSlots(ctx, tournamentID, false)
}

// ListPublicSlots is the same schedule filtered to what a visitor should
// actually see: matches belonging to a public event, and only slots that
// have a match assigned at all — an empty held slot ("court blocked, nothing
// scheduled yet") isn't informative to a spectator and shouldn't appear on a
// public order-of-play list.
func (r *Repository) ListPublicSlots(ctx context.Context, tournamentID uuid.UUID) ([]Slot, error) {
	return r.listSlots(ctx, tournamentID, true)
}

func (r *Repository) listSlots(ctx context.Context, tournamentID uuid.UUID, publicOnly bool) ([]Slot, error) {
	q := `
		SELECT s.id, s.court_id, c.name, s.match_id,
		       (SELECT string_agg(p.display_name, ' vs ' ORDER BY mp.slot)
		        FROM match_participants mp
		        LEFT JOIN participants p ON p.id = mp.participant_id
		        WHERE mp.match_id = s.match_id) AS match_label,
		       m.event_id, COALESCE(e.public_display_name, e.name), m.status,
		       s.starts_at, s.ends_at
		FROM schedule_slots s
		JOIN courts c ON c.id = s.court_id
		LEFT JOIN matches m ON m.id = s.match_id
		LEFT JOIN events e ON e.id = m.event_id
		WHERE s.tournament_id = $1`
	if publicOnly {
		q += ` AND s.match_id IS NOT NULL AND e.is_public = true`
	}
	q += ` ORDER BY s.starts_at, c.sort_order`

	rows, err := r.pool.Query(ctx, q, tournamentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Slot{}
	for rows.Next() {
		var s Slot
		if err := rows.Scan(
			&s.ID, &s.CourtID, &s.CourtName, &s.MatchID, &s.MatchLabel,
			&s.EventID, &s.EventName, &s.MatchStatus,
			&s.StartsAt, &s.EndsAt,
		); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// slotWrite is one validated create-or-update of a schedule slot.
type slotWrite struct {
	slotID       *uuid.UUID // nil = create
	tournamentID uuid.UUID
	courtID      uuid.UUID
	matchID      *uuid.UUID
	startsAt     time.Time
	endsAt       time.Time
	overrideRest bool
	actor        uuid.UUID
	orgID        uuid.UUID
}

// conflictLabelCols is the SELECT list shared by both conflict queries.
const conflictLabelCols = `
	s.id, s.court_id, c.name, s.match_id,
	(SELECT string_agg(p.display_name, ' vs ' ORDER BY mp.slot)
	 FROM match_participants mp
	 LEFT JOIN participants p ON p.id = mp.participant_id
	 WHERE mp.match_id = s.match_id),
	s.starts_at, s.ends_at`

func scanConflicts(rows pgx.Rows, typ string) ([]Conflict, error) {
	defer rows.Close()
	var out []Conflict
	for rows.Next() {
		var cf Conflict
		cf.Type = typ
		if err := rows.Scan(&cf.SlotID, &cf.CourtID, &cf.CourtName, &cf.MatchID,
			&cf.MatchLabel, &cf.StartsAt, &cf.EndsAt); err != nil {
			return nil, err
		}
		out = append(out, cf)
	}
	return out, rows.Err()
}

// validateSlotWrite runs the conflict checks inside the caller's transaction.
// Both the court and participant checks use half-open '[)' semantics, so
// back-to-back slots never conflict. Decided matches (completed/walkover/
// retired/cancelled/bye) no longer occupy their pairs, so they are ignored by
// the participant checks; their slots still block the court itself.
func validateSlotWrite(ctx context.Context, tx pgx.Tx, w slotWrite) error {
	courtRows, err := tx.Query(ctx, `
		SELECT `+conflictLabelCols+`
		FROM schedule_slots s JOIN courts c ON c.id = s.court_id
		WHERE s.court_id = $1
		  AND ($2::uuid IS NULL OR s.id <> $2)
		  AND tstzrange(s.starts_at, s.ends_at, '[)') && tstzrange($3, $4, '[)')
		ORDER BY s.starts_at`,
		w.courtID, w.slotID, w.startsAt, w.endsAt)
	if err != nil {
		return err
	}
	hard, err := scanConflicts(courtRows, "court_overlap")
	if err != nil {
		return err
	}

	var rest []Conflict
	if w.matchID != nil {
		// One query over the rest-expanded window; classified in Go into hard
		// participant overlaps vs rest-buffer warnings by actual time overlap.
		candRows, err := tx.Query(ctx, `
			SELECT DISTINCT `+conflictLabelCols+`
			FROM schedule_slots s
			JOIN courts c ON c.id = s.court_id
			JOIN matches m2 ON m2.id = s.match_id
			JOIN match_participants mp2 ON mp2.match_id = m2.id
			WHERE ($1::uuid IS NULL OR s.id <> $1)
			  AND m2.status NOT IN ('completed', 'walkover', 'retired', 'cancelled', 'bye')
			  AND mp2.participant_id IN (
			      SELECT participant_id FROM match_participants
			      WHERE match_id = $2 AND participant_id IS NOT NULL)
			  AND tstzrange(s.starts_at, s.ends_at, '[)') && tstzrange($3, $4, '[)')
			ORDER BY s.starts_at`,
			w.slotID, *w.matchID, w.startsAt.Add(-restBuffer), w.endsAt.Add(restBuffer))
		if err != nil {
			return err
		}
		cands, err := scanConflicts(candRows, "")
		if err != nil {
			return err
		}
		for _, cf := range cands {
			if cf.StartsAt.Before(w.endsAt) && w.startsAt.Before(cf.EndsAt) {
				cf.Type = "participant_overlap"
				hard = append(hard, cf)
			} else {
				cf.Type = "rest_buffer"
				rest = append(rest, cf)
			}
		}
	}

	if len(hard) > 0 || (len(rest) > 0 && !w.overrideRest) {
		return &ConflictError{Hard: hard, Rest: rest}
	}
	return nil
}

// stateConflict translates the DB's last-resort guards into ErrStateConflict:
// 23P01 = exclusion constraint (a concurrent insert won the court).
func stateConflict(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23P01" {
		return ErrStateConflict
	}
	return err
}

// WriteSlot creates (slotID nil) or updates a slot inside one transaction:
// per-tournament advisory lock -> conflict validation -> write + match stamp
// -> audit -> commit. A validation failure rolls back with no rows touched.
func (r *Repository) WriteSlot(ctx context.Context, w slotWrite) (*Slot, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	// Serialize schedule writes per tournament so two concurrent submissions
	// can't both pass validation before either commits. The DB exclusion
	// constraint still backstops court overlap; this lock is what makes the
	// participant/rest checks race-free too.
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))`,
		w.tournamentID.String()); err != nil {
		return nil, err
	}

	var prevMatch *uuid.UUID
	var before map[string]any
	if w.slotID != nil {
		var pStarts, pEnds time.Time
		var pCourt uuid.UUID
		err := tx.QueryRow(ctx, `
			SELECT match_id, court_id, starts_at, ends_at
			FROM schedule_slots WHERE id = $1 FOR UPDATE`, *w.slotID).
			Scan(&prevMatch, &pCourt, &pStarts, &pEnds)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		before = map[string]any{
			"court_id": pCourt, "match_id": uuidPtrStr(prevMatch),
			"starts_at": pStarts, "ends_at": pEnds,
		}
	}

	if err := validateSlotWrite(ctx, tx, w); err != nil {
		return nil, err
	}

	var slotID uuid.UUID
	action := "schedule.slot.update"
	if w.slotID == nil {
		action = "schedule.slot.create"
		if err := tx.QueryRow(ctx, `
			INSERT INTO schedule_slots (tournament_id, court_id, match_id, starts_at, ends_at)
			VALUES ($1, $2, $3, $4, $5) RETURNING id`,
			w.tournamentID, w.courtID, w.matchID, w.startsAt, w.endsAt).Scan(&slotID); err != nil {
			return nil, stateConflict(err)
		}
	} else {
		slotID = *w.slotID
		if _, err := tx.Exec(ctx, `
			UPDATE schedule_slots SET court_id = $2, match_id = $3, starts_at = $4, ends_at = $5
			WHERE id = $1`, slotID, w.courtID, w.matchID, w.startsAt, w.endsAt); err != nil {
			return nil, stateConflict(err)
		}
	}

	// Un-stamp a previously assigned match that's gone or swapped out, then
	// stamp the new one (scheduled_at/court mirror for bracket/match views).
	if prevMatch != nil && (w.matchID == nil || *prevMatch != *w.matchID) {
		if _, err := tx.Exec(ctx,
			`UPDATE matches SET scheduled_at = NULL, court_id = NULL,
			   status = CASE WHEN status = 'scheduled' THEN 'pending'::match_status ELSE status END
			 WHERE id = $1`, *prevMatch); err != nil {
			return nil, err
		}
	}
	if w.matchID != nil {
		if _, err := tx.Exec(ctx,
			`UPDATE matches SET scheduled_at = $2, court_id = $3,
			   status = CASE WHEN status = 'pending' THEN 'scheduled'::match_status ELSE status END
			 WHERE id = $1`, *w.matchID, w.startsAt, w.courtID); err != nil {
			return nil, err
		}
	}

	if err := r.audit.RecordTx(ctx, tx, audit.Entry{
		ActorUserID:  w.actor,
		OrgID:        &w.orgID,
		TournamentID: &w.tournamentID,
		Action:       action,
		TargetType:   "schedule_slot",
		TargetID:     slotID.String(),
		Diff: map[string]any{
			"before": before,
			"after": map[string]any{
				"court_id": w.courtID, "match_id": uuidPtrStr(w.matchID),
				"starts_at": w.startsAt, "ends_at": w.endsAt,
			},
			"rest_override": w.overrideRest,
		},
	}); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, stateConflict(err)
	}
	return r.getSlot(ctx, slotID)
}

func uuidPtrStr(u *uuid.UUID) any {
	if u == nil {
		return nil
	}
	return u.String()
}

func (r *Repository) getSlot(ctx context.Context, id uuid.UUID) (*Slot, error) {
	var s Slot
	err := r.pool.QueryRow(ctx, `
		SELECT s.id, s.court_id, c.name, s.match_id,
		       (SELECT string_agg(p.display_name, ' vs ' ORDER BY mp.slot)
		        FROM match_participants mp LEFT JOIN participants p ON p.id = mp.participant_id
		        WHERE mp.match_id = s.match_id),
		       s.starts_at, s.ends_at
		FROM schedule_slots s JOIN courts c ON c.id = s.court_id
		WHERE s.id = $1`, id).
		Scan(&s.ID, &s.CourtID, &s.CourtName, &s.MatchID, &s.MatchLabel, &s.StartsAt, &s.EndsAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &s, err
}

// DeleteSlot removes a slot, un-stamps its match (unless another slot still
// references it), and audits the removal — all in one transaction.
func (r *Repository) DeleteSlot(ctx context.Context, id uuid.UUID, actor uuid.UUID, orgID uuid.UUID) (uuid.UUID, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	defer tx.Rollback(ctx)

	var tournamentID, courtID uuid.UUID
	var matchID *uuid.UUID
	var startsAt, endsAt time.Time
	err = tx.QueryRow(ctx, `
		DELETE FROM schedule_slots WHERE id = $1
		RETURNING tournament_id, court_id, match_id, starts_at, ends_at`, id).
		Scan(&tournamentID, &courtID, &matchID, &startsAt, &endsAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrNotFound
	}
	if err != nil {
		return uuid.Nil, err
	}

	if matchID != nil {
		if _, err := tx.Exec(ctx, `
			UPDATE matches SET scheduled_at = NULL, court_id = NULL,
			  status = CASE WHEN status = 'scheduled' THEN 'pending'::match_status ELSE status END
			WHERE id = $1
			  AND NOT EXISTS (SELECT 1 FROM schedule_slots WHERE match_id = $1)`,
			*matchID); err != nil {
			return uuid.Nil, err
		}
	}

	if err := r.audit.RecordTx(ctx, tx, audit.Entry{
		ActorUserID:  actor,
		OrgID:        &orgID,
		TournamentID: &tournamentID,
		Action:       "schedule.slot.delete",
		TargetType:   "schedule_slot",
		TargetID:     id.String(),
		Diff: map[string]any{
			"before": map[string]any{
				"court_id": courtID, "match_id": uuidPtrStr(matchID),
				"starts_at": startsAt, "ends_at": endsAt,
			},
		},
	}); err != nil {
		return uuid.Nil, err
	}
	return tournamentID, tx.Commit(ctx)
}

// SlotTournamentOrg returns the owning org of a slot's tournament.
func (r *Repository) SlotTournamentOrg(ctx context.Context, slotID uuid.UUID) (uuid.UUID, error) {
	var orgID uuid.UUID
	err := r.pool.QueryRow(ctx, `
		SELECT t.org_id FROM schedule_slots s
		JOIN tournaments t ON t.id = s.tournament_id WHERE s.id = $1`, slotID).Scan(&orgID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrNotFound
	}
	return orgID, err
}

// PublishedTournamentBySlug returns the tournament id for a published slug (for
// public schedule reads), or ErrNotFound.
func (r *Repository) PublishedTournamentBySlug(ctx context.Context, slug string) (uuid.UUID, error) {
	var id uuid.UUID
	err := r.pool.QueryRow(ctx,
		`SELECT id FROM tournaments WHERE slug = $1 AND status = 'published'`, slug).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrNotFound
	}
	return id, err
}
