package match

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("match not found")

// SetScore is one set's games (+ optional tiebreak) for both sides.
type SetScore struct {
	SetNumber  int  `json:"set_number"`
	P1Games    int  `json:"p1_games"`
	P2Games    int  `json:"p2_games"`
	P1Tiebreak *int `json:"p1_tiebreak"`
	P2Tiebreak *int `json:"p2_tiebreak"`
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
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
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
	out := make([]Match, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	return out, nil
}

// EventOrgAndSlug returns the owning org and tournament slug for a match, used
// for authorization and to pick the SSE topic.
func (r *Repository) EventOrgAndSlug(ctx context.Context, matchID uuid.UUID) (orgID uuid.UUID, slug string, err error) {
	err = r.pool.QueryRow(ctx, `
		SELECT t.org_id, t.slug
		FROM matches m
		JOIN events e ON e.id = m.event_id
		JOIN tournaments t ON t.id = e.tournament_id
		WHERE m.id = $1`, matchID).Scan(&orgID, &slug)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, "", ErrNotFound
	}
	return orgID, slug, err
}

// SaveScore persists set scores, sets the winner + status, and advances the
// winner into the next match — all in one transaction. participant1/2 are the
// slot-1 and slot-2 participant ids (may be nil). winnerID is the resolved
// winner. It returns nothing; caller re-reads via Get for the response.
func (r *Repository) SaveScore(
	ctx context.Context,
	matchID uuid.UUID,
	sets []SetScore,
	status string,
	winnerID *uuid.UUID,
	nextMatchID *uuid.UUID,
	nextSlot *int,
) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// Replace the set rows.
	if _, err := tx.Exec(ctx, `DELETE FROM match_scores WHERE match_id = $1`, matchID); err != nil {
		return err
	}
	for _, s := range sets {
		if _, err := tx.Exec(ctx, `
			INSERT INTO match_scores (match_id, set_number, p1_games, p2_games, p1_tiebreak, p2_tiebreak)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			matchID, s.SetNumber, s.P1Games, s.P2Games, s.P1Tiebreak, s.P2Tiebreak); err != nil {
			return err
		}
	}

	// Update the match: status, winner, timestamps.
	if _, err := tx.Exec(ctx, `
		UPDATE matches SET
			status = $2::match_status,
			winner_participant_id = $3,
			started_at = COALESCE(started_at, now()),
			completed_at = CASE WHEN $2 = 'completed' THEN now() ELSE NULL END
		WHERE id = $1`, matchID, status, winnerID); err != nil {
		return err
	}

	// Advance the winner into the next match's slot, if this match is done and
	// feeds another. This makes the bracket progress automatically.
	if status == "completed" && winnerID != nil && nextMatchID != nil && nextSlot != nil {
		if _, err := tx.Exec(ctx, `
			UPDATE match_participants SET participant_id = $1
			WHERE match_id = $2 AND slot = $3`, *winnerID, *nextMatchID, *nextSlot); err != nil {
			return err
		}
	}

	return tx.Commit(ctx)
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
