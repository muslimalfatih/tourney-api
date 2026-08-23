package match

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/muslimalfatih/tourney-api/internal/audit"
	"github.com/muslimalfatih/tourney-api/internal/draw"
)

var ErrNotFound = errors.New("match not found")

// SetScore is one set's games (+ optional tiebreak) for both sides.
// SetScore is one set on the wire and in match_scores. Wire names are the
// Phase 3.4 canonical a/b fields; the database columns keep their original
// p1/p2 names (no schema rename). Tiebreak metadata is separate from games:
// a 7-6 set stores games 7-6 plus the tiebreak points here.
type SetScore struct {
	SetNumber  int  `json:"set_number"`
	P1Games    int  `json:"games_a"`
	P2Games    int  `json:"games_b"`
	P1Tiebreak *int `json:"tiebreak_a"`
	P2Tiebreak *int `json:"tiebreak_b"`
}

// SlotView is a match slot with the resolved participant (nil until fed).
type SlotView struct {
	Slot          int        `json:"slot"`
	ParticipantID *uuid.UUID `json:"participant_id"`
	DisplayName   *string    `json:"display_name"`
	Seed          *int       `json:"seed"`
}

// Match is the full match read model for detail + scoring.
type Match struct {
	ID                  uuid.UUID  `json:"id"`
	EventID             uuid.UUID  `json:"event_id"`
	MatchNo             int        `json:"match_no"`
	Status              string     `json:"status"`
	ScheduledAt         *time.Time `json:"scheduled_at"`
	StartedAt           *time.Time `json:"started_at"`
	CompletedAt         *time.Time `json:"completed_at"`
	WinnerParticipantID *uuid.UUID `json:"winner_participant_id"`
	NextMatchID         *uuid.UUID `json:"next_match_id"`
	NextSlot            *int       `json:"next_slot"`
	Participants        []SlotView `json:"participants"`
	Sets                []SetScore `json:"sets"`
}

type Repository struct {
	pool  *pgxpool.Pool
	draws *draw.Service
	audit *audit.Service
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool, draws: draw.NewService(pool), audit: audit.NewService(pool)}
}

// Get loads a match with its slots (participant names/seeds) and set scores.
func (r *Repository) Get(ctx context.Context, id uuid.UUID) (*Match, error) {
	m := Match{Participants: []SlotView{}, Sets: []SetScore{}}
	err := r.pool.QueryRow(ctx, `
		SELECT id, event_id, match_no, status, scheduled_at, started_at, completed_at,
		       winner_participant_id, next_match_id, next_slot
		FROM matches WHERE id = $1`, id).Scan(
		&m.ID, &m.EventID, &m.MatchNo, &m.Status, &m.ScheduledAt, &m.StartedAt, &m.CompletedAt,
		&m.WinnerParticipantID, &m.NextMatchID, &m.NextSlot)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	// Slots.
	srows, err := r.pool.Query(ctx, `
		SELECT mp.slot, mp.participant_id, p.display_name, p.seed
		FROM match_participants mp
		LEFT JOIN participants p ON p.id = mp.participant_id
		WHERE mp.match_id = $1 ORDER BY mp.slot`, id)
	if err != nil {
		return nil, err
	}
	defer srows.Close()
	for srows.Next() {
		var s SlotView
		if err := srows.Scan(&s.Slot, &s.ParticipantID, &s.DisplayName, &s.Seed); err != nil {
			return nil, err
		}
		m.Participants = append(m.Participants, s)
	}
	if err := srows.Err(); err != nil {
		return nil, err
	}

	// Sets.
	scrows, err := r.pool.Query(ctx, `
		SELECT set_number, p1_games, p2_games, p1_tiebreak, p2_tiebreak
		FROM match_scores WHERE match_id = $1 ORDER BY set_number`, id)
	if err != nil {
		return nil, err
	}
	defer scrows.Close()
	for scrows.Next() {
		var s SetScore
		if err := scrows.Scan(&s.SetNumber, &s.P1Games, &s.P2Games, &s.P1Tiebreak, &s.P2Tiebreak); err != nil {
			return nil, err
		}
		m.Sets = append(m.Sets, s)
	}
	return &m, scrows.Err()
}

// ListByEvent returns an event's matches (headers only) for the organizer list.
func (r *Repository) ListByEvent(ctx context.Context, eventID uuid.UUID) ([]Match, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT m.id, m.event_id, m.match_no, m.status, m.scheduled_at, m.started_at, m.completed_at,
		       m.winner_participant_id, m.next_match_id, m.next_slot,
		       mp.slot, mp.participant_id, p.display_name, p.seed
		FROM matches m
		LEFT JOIN match_participants mp ON mp.match_id = m.id
		LEFT JOIN participants p ON p.id = mp.participant_id
		WHERE m.event_id = $1
		ORDER BY m.match_no, mp.slot`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	order := []uuid.UUID{}
	byID := map[uuid.UUID]*Match{}
	for rows.Next() {
		var (
			mid                       uuid.UUID
			eid                       uuid.UUID
			no                        int
			status                    string
			sched, started, completed *time.Time
			winner, next              *uuid.UUID
			nextSlot                  *int
			slot                      *int
			pid                       *uuid.UUID
			name                      *string
			seed                      *int
		)
		if err := rows.Scan(&mid, &eid, &no, &status, &sched, &started, &completed,
			&winner, &next, &nextSlot, &slot, &pid, &name, &seed); err != nil {
			return nil, err
		}
		m, ok := byID[mid]
		if !ok {
			m = &Match{ID: mid, EventID: eid, MatchNo: no, Status: status, ScheduledAt: sched,
				StartedAt: started, CompletedAt: completed, WinnerParticipantID: winner,
				NextMatchID: next, NextSlot: nextSlot, Participants: []SlotView{}, Sets: []SetScore{}}
			byID[mid] = m
			order = append(order, mid)
		}
		if slot != nil {
			m.Participants = append(m.Participants, SlotView{Slot: *slot, ParticipantID: pid, DisplayName: name, Seed: seed})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Hydrate set scores in one sweep. Without this the organizer matches
	// page showed blank score columns and seeded EMPTY correction forms for
	// played matches (found by the Phase 4E browser suite) — the list was
	// "headers only" while every consumer treated Sets as real.
	setRows, err := r.pool.Query(ctx, `
		SELECT s.match_id, s.set_number, s.p1_games, s.p2_games, s.p1_tiebreak, s.p2_tiebreak
		FROM match_scores s
		JOIN matches m ON m.id = s.match_id
		WHERE m.event_id = $1
		ORDER BY s.match_id, s.set_number`, eventID)
	if err != nil {
		return nil, err
	}
	defer setRows.Close()
	for setRows.Next() {
		var mid uuid.UUID
		var st SetScore
		if err := setRows.Scan(&mid, &st.SetNumber, &st.P1Games, &st.P2Games, &st.P1Tiebreak, &st.P2Tiebreak); err != nil {
			return nil, err
		}
		if m, ok := byID[mid]; ok {
			m.Sets = append(m.Sets, st)
		}
	}
	if err := setRows.Err(); err != nil {
		return nil, err
	}

	out := make([]Match, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	return out, nil
}

// EventOrgAndSlug returns the owning org, tournament slug, and public
// visibility for a match: `published` is the tournament's publication state,
// `eventPublic` the division's is_public flag. One query serves authorization
// (org), SSE topic selection (slug), and the public-visibility gates — public
// reads and broadcasts require published && eventPublic.
func (r *Repository) EventOrgAndSlug(ctx context.Context, matchID uuid.UUID) (orgID uuid.UUID, tournamentID uuid.UUID, slug string, published, eventPublic bool, err error) {
	err = r.pool.QueryRow(ctx, `
		SELECT t.org_id, t.id, t.slug, t.status = 'published', e.is_public
		FROM matches m
		JOIN events e ON e.id = m.event_id
		JOIN tournaments t ON t.id = e.tournament_id
		WHERE m.id = $1`, matchID).Scan(&orgID, &tournamentID, &slug, &published, &eventPublic)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, uuid.Nil, "", false, false, ErrNotFound
	}
	return orgID, tournamentID, slug, published, eventPublic, err
}

// DownstreamLockedError reports downstream slots whose value a correction
// would change while their match is live, completed, or already carries
// scores. The whole transaction aborts — nothing is applied or silently
// skipped — and the handler maps this to a 409 with the affected slots.
type DownstreamLockedError struct {
	Locked []draw.LockedSlot
}

func (e *DownstreamLockedError) Error() string {
	return fmt.Sprintf("%d downstream slot(s) locked by matches already in progress", len(e.Locked))
}

// ScoringForMatch returns the raw scoring config of the match's division.
func (r *Repository) ScoringForMatch(ctx context.Context, matchID uuid.UUID) ([]byte, error) {
	var raw []byte
	err := r.pool.QueryRow(ctx, `
		SELECT e.scoring FROM matches m JOIN events e ON e.id = m.event_id WHERE m.id = $1`,
		matchID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return raw, err
}

// SaveScoreInput carries a score submission or correction plus the audit
// context (actor / org / tournament) the transaction records.
type SaveScoreInput struct {
	MatchID      uuid.UUID
	Sets         []SetScore
	Status       string // live | completed | walkover | retired | cancelled
	Completion   string
	WinnerID     *uuid.UUID
	Actor        uuid.UUID
	OrgID        uuid.UUID
	TournamentID uuid.UUID
}

// SaveScore persists a score submission or correction in ONE transaction
// (Refactor Phase 3.3):
//
//	lock source row → capture before-state → replace sets → update match →
//	rebuild downstream winner-feed slots (409 if any changed slot's match has
//	started) → rebuild this group's placement slots when standings are
//	affected (same 409 rule) → write the audit record → commit.
//
// SSE emission is the caller's job, strictly after commit. Recursion note:
// downstream slots are found by source_match_id; because a locked (started)
// downstream match aborts the whole correction, any downstream match we DO
// touch cannot itself have completed — so its own feeds hold no stale winner
// to rebuild, and the rebuild provably terminates after the direct consumers.
func (r *Repository) SaveScore(ctx context.Context, in SaveScoreInput) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// Step 1: lock the source match and capture its prior state.
	var (
		oldStatus string
		oldWinner *uuid.UUID
		groupID   *uuid.UUID
	)
	err = tx.QueryRow(ctx, `
		SELECT status, winner_participant_id, group_id
		FROM matches WHERE id = $1 FOR UPDATE`, in.MatchID).
		Scan(&oldStatus, &oldWinner, &groupID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	oldSets, err := r.setsTx(ctx, tx, in.MatchID)
	if err != nil {
		return err
	}

	// Replace the set rows.
	if _, err := tx.Exec(ctx, `DELETE FROM match_scores WHERE match_id = $1`, in.MatchID); err != nil {
		return err
	}
	for _, st := range in.Sets {
		if _, err := tx.Exec(ctx, `
			INSERT INTO match_scores (match_id, set_number, p1_games, p2_games, p1_tiebreak, p2_tiebreak)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			in.MatchID, st.SetNumber, st.P1Games, st.P2Games, st.P1Tiebreak, st.P2Tiebreak); err != nil {
			return err
		}
	}

	// Update the match row. completed_at is preserved across a same-status
	// re-score and cleared when a correction reopens the match.
	if _, err := tx.Exec(ctx, `
		UPDATE matches SET
			status = $2::match_status,
			winner_participant_id = $3,
			started_at = COALESCE(started_at, now()),
			completed_at = CASE WHEN $2 IN ('completed','walkover','retired','cancelled')
			                    THEN COALESCE(completed_at, now()) ELSE NULL END
		WHERE id = $1`, in.MatchID, in.Status, in.WinnerID); err != nil {
		return err
	}

	// Steps 3-8: rebuild direct winner-feed consumers when the winner changed
	// (including changing to nil on un-complete).
	downstreamChanged := 0
	if !uuidPtrEq(oldWinner, in.WinnerID) {
		rows, err := tx.Query(ctx, `
			SELECT mp.id, mp.participant_id, m.id, m.match_no, m.status, mp.slot,
			       EXISTS(SELECT 1 FROM match_scores ms WHERE ms.match_id = m.id)
			FROM match_participants mp
			JOIN matches m ON m.id = mp.match_id
			WHERE mp.source_match_id = $1 AND mp.source_type = 'match_winner'
			FOR UPDATE OF m, mp`, in.MatchID)
		if err != nil {
			return err
		}
		type consumer struct {
			slotRowID uuid.UUID
			current   *uuid.UUID
			matchID   uuid.UUID
			matchNo   int
			status    string
			slot      int
			hasScores bool
		}
		var consumers []consumer
		for rows.Next() {
			var c consumer
			if err := rows.Scan(&c.slotRowID, &c.current, &c.matchID, &c.matchNo, &c.status, &c.slot, &c.hasScores); err != nil {
				rows.Close()
				return err
			}
			consumers = append(consumers, c)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		var locked []draw.LockedSlot
		for _, c := range consumers {
			if uuidPtrEq(c.current, in.WinnerID) {
				continue // value unchanged — not affected, never locks
			}
			// "bye" is structural (a pad match, decided by construction), never
			// "started" — its auto-advance is rebuilt recursively like any feed.
			if (c.status != "pending" && c.status != "scheduled" && c.status != "bye") || c.hasScores {
				locked = append(locked, draw.LockedSlot{MatchID: c.matchID, MatchNo: c.matchNo, Slot: c.slot, Status: c.status})
			}
		}
		if len(locked) > 0 {
			return &DownstreamLockedError{Locked: locked}
		}
		for _, c := range consumers {
			if uuidPtrEq(c.current, in.WinnerID) {
				continue
			}
			if _, err := tx.Exec(ctx,
				`UPDATE match_participants SET participant_id = $1 WHERE id = $2`,
				in.WinnerID, c.slotRowID); err != nil {
				return err
			}
			downstreamChanged++
		}
	}

	// Step 9: a group match moving into or out of 'completed' changes that
	// group's standings — rebuild its placement slots under the same lock rule.
	groupChanged := 0
	if groupID != nil && (decidedStatus(oldStatus) || decidedStatus(in.Status)) {
		var remaining int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM matches
			WHERE group_id = $1
			  AND status NOT IN ('completed','bye','walkover','retired','cancelled')`,
			*groupID).Scan(&remaining); err != nil {
			return err
		}
		changed, locked, err := r.draws.RebuildGroupPlacements(ctx, tx, *groupID, remaining == 0)
		if err != nil {
			return err
		}
		if len(locked) > 0 {
			return &DownstreamLockedError{Locked: locked}
		}
		groupChanged = changed
	}

	// Step 10: audit, inside the transaction, with the before-state.
	if err := r.audit.RecordTx(ctx, tx, audit.Entry{
		OrgID:        &in.OrgID,
		ActorUserID:  in.Actor,
		TournamentID: &in.TournamentID,
		Action:       "match.score",
		TargetType:   "match",
		TargetID:     in.MatchID.String(),
		Diff: map[string]any{
			"before":             map[string]any{"status": oldStatus, "winner": uuidPtrStr(oldWinner), "sets": oldSets},
			"after":              map[string]any{"status": in.Status, "winner": uuidPtrStr(in.WinnerID), "sets": in.Sets},
			"completion":         in.Completion,
			"downstream_rebuilt": downstreamChanged,
			"placements_rebuilt": groupChanged,
		},
	}); err != nil {
		return err
	}

	// Step 11: commit. SSE (step 12) happens in the handler, post-commit.
	return tx.Commit(ctx)
}

// setsTx reads a match's current set rows inside the transaction.
func (r *Repository) setsTx(ctx context.Context, tx pgx.Tx, matchID uuid.UUID) ([]SetScore, error) {
	rows, err := tx.Query(ctx, `
		SELECT set_number, p1_games, p2_games, p1_tiebreak, p2_tiebreak
		FROM match_scores WHERE match_id = $1 ORDER BY set_number`, matchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SetScore{}
	for rows.Next() {
		var st SetScore
		if err := rows.Scan(&st.SetNumber, &st.P1Games, &st.P2Games, &st.P1Tiebreak, &st.P2Tiebreak); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

func uuidPtrEq(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func uuidPtrStr(u *uuid.UUID) any {
	if u == nil {
		return nil
	}
	return u.String()
}

// SetStatus updates only the match status (e.g. mark live).
func (r *Repository) SetStatus(ctx context.Context, matchID uuid.UUID, status string) error {
	ct, err := r.pool.Exec(ctx, `
		UPDATE matches SET
			status = $2::match_status,
			started_at = CASE WHEN $2 = 'live' THEN COALESCE(started_at, now()) ELSE started_at END
		WHERE id = $1`, matchID, status)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
