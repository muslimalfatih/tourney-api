package match

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/muslimalfatih/laga-api/internal/draw"
)

var (
	ErrForbidden = errors.New("not permitted")
	ErrNoScores  = errors.New("at least one set is required")
	ErrNoWinner  = errors.New("scores do not determine a winner")
)

// ScoreRequest is the organizer's score submission. complete=true finalizes the
// match, computes the winner from the sets, and advances the bracket.
type ScoreRequest struct {
	Sets     []SetScore `json:"sets" binding:"required"`
	Complete bool       `json:"complete"`
}

type StatusRequest struct {
	Status string `json:"status" binding:"required,oneof=scheduled live completed"`
}

type Service struct {
	repo *Repository
	draw *draw.Service
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{repo: NewRepository(pool), draw: draw.NewService(pool)}
}

func (s *Service) Get(ctx context.Context, id uuid.UUID) (*Match, error) {
	return s.repo.Get(ctx, id)
}

func (s *Service) ListForEvent(ctx context.Context, eventID uuid.UUID) ([]Match, error) {
	return s.repo.ListByEvent(ctx, eventID)
}

// SlugForMatch returns the SSE topic (tournament slug) for a match.
func (s *Service) SlugForMatch(ctx context.Context, matchID uuid.UUID) (string, error) {
	_, slug, err := s.repo.EventOrgAndSlug(ctx, matchID)
	return slug, err
}

// SubmitScore authorizes, computes the winner (when completing), persists, and
// advances the bracket. Returns the refreshed match + the tournament slug for
// SSE broadcast.
func (s *Service) SubmitScore(ctx context.Context, matchID uuid.UUID, orgID *uuid.UUID, req ScoreRequest) (*Match, string, error) {
	ownerOrg, slug, err := s.repo.EventOrgAndSlug(ctx, matchID)
	if err != nil {
		return nil, "", err
	}
	if orgID != nil && ownerOrg != *orgID {
		return nil, "", ErrForbidden
	}
	if len(req.Sets) == 0 {
		return nil, "", ErrNoScores
	}

	current, err := s.repo.Get(ctx, matchID)
	if err != nil {
		return nil, "", err
	}

	status := "live"
	var winnerID *uuid.UUID
	if req.Complete {
		status = "completed"
		side := decideWinner(req.Sets)
		if side == 0 {
			return nil, "", ErrNoWinner
		}
		winnerID = participantForSlot(current, side)
		if winnerID == nil {
			return nil, "", ErrNoWinner
		}
	}

	if err := s.repo.SaveScore(ctx, matchID, req.Sets, status, winnerID, current.NextMatchID, current.NextSlot); err != nil {
		return nil, "", err
	}

	// If this completed a group-knockout group stage, auto-fill the knockout.
	// Best-effort: a failure here must not fail the score submission.
	if req.Complete {
		_, _ = s.draw.MaybeResolveGroups(ctx, matchID)
	}

	updated, err := s.repo.Get(ctx, matchID)
	if err != nil {
		return nil, "", err
	}
	return updated, slug, nil
}

// SetStatus authorizes and updates a match's status (e.g. mark live).
func (s *Service) SetStatus(ctx context.Context, matchID uuid.UUID, orgID *uuid.UUID, req StatusRequest) (*Match, string, error) {
	ownerOrg, slug, err := s.repo.EventOrgAndSlug(ctx, matchID)
	if err != nil {
		return nil, "", err
	}
	if orgID != nil && ownerOrg != *orgID {
		return nil, "", ErrForbidden
	}
	if err := s.repo.SetStatus(ctx, matchID, req.Status); err != nil {
		return nil, "", err
	}
	m, err := s.repo.Get(ctx, matchID)
	if err != nil {
		return nil, "", err
	}
	return m, slug, nil
}

// decideWinner counts sets won by each side (tennis: most sets wins; a set is
// won by whoever has more games, tiebreak breaks equal games). Returns 1, 2, or
// 0 (undecided/tie).
func decideWinner(sets []SetScore) int {
	p1, p2 := 0, 0
	for _, s := range sets {
		switch {
		case s.P1Games > s.P2Games:
			p1++
		case s.P2Games > s.P1Games:
			p2++
		default:
			// equal games → decide by tiebreak if present
			if s.P1Tiebreak != nil && s.P2Tiebreak != nil {
				if *s.P1Tiebreak > *s.P2Tiebreak {
					p1++
				} else if *s.P2Tiebreak > *s.P1Tiebreak {
					p2++
				}
			}
		}
	}
	switch {
	case p1 > p2:
		return 1
	case p2 > p1:
		return 2
	default:
		return 0
	}
}

// participantForSlot returns the participant id occupying a slot (1 or 2).
func participantForSlot(m *Match, slot int) *uuid.UUID {
	for _, p := range m.Participants {
		if p.Slot == slot {
			return p.ParticipantID
		}
	}
	return nil
}
