package schedule

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type CreateCourtRequest struct {
	Name      string `json:"name" binding:"required"`
	SortOrder int    `json:"sort_order"`
}

type CreateSlotRequest struct {
	TournamentID string  `json:"tournament_id" binding:"required"`
	CourtID      string  `json:"court_id" binding:"required"`
	MatchID      *string `json:"match_id"`
	StartsAt     string  `json:"starts_at" binding:"required"` // RFC3339
	EndsAt       string  `json:"ends_at" binding:"required"`
}

// UpdateSlotRequest edits an existing slot's court, assigned match, and time.
// No tournament_id — the slot is looked up by its own id and re-authorized.
type UpdateSlotRequest struct {
	CourtID  string  `json:"court_id" binding:"required"`
	MatchID  *string `json:"match_id"`
	StartsAt string  `json:"starts_at" binding:"required"` // RFC3339
	EndsAt   string  `json:"ends_at" binding:"required"`
}

type Service struct {
	repo *Repository
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{repo: NewRepository(pool)}
}

func (s *Service) authorizeTournament(ctx context.Context, tournamentID uuid.UUID, orgID *uuid.UUID) error {
	owner, err := s.repo.TournamentOrg(ctx, tournamentID)
	if err != nil {
		return err
	}
	if orgID != nil && owner != *orgID {
		return ErrForbidden
	}
	return nil
}

func (s *Service) ListCourts(ctx context.Context, tournamentID uuid.UUID, orgID *uuid.UUID) ([]Court, error) {
	if err := s.authorizeTournament(ctx, tournamentID, orgID); err != nil {
		return nil, err
	}
	return s.repo.ListCourts(ctx, tournamentID)
}

func (s *Service) CreateCourt(ctx context.Context, tournamentID uuid.UUID, orgID *uuid.UUID, req CreateCourtRequest) (*Court, error) {
	if err := s.authorizeTournament(ctx, tournamentID, orgID); err != nil {
		return nil, err
	}
	return s.repo.CreateCourt(ctx, tournamentID, strings.TrimSpace(req.Name), req.SortOrder)
}

func (s *Service) ListSlots(ctx context.Context, tournamentID uuid.UUID, orgID *uuid.UUID) ([]Slot, error) {
	if err := s.authorizeTournament(ctx, tournamentID, orgID); err != nil {
		return nil, err
	}
	return s.repo.ListSlots(ctx, tournamentID)
}

// ListPublicSlots returns the schedule for a published tournament by slug.
func (s *Service) ListPublicSlots(ctx context.Context, slug string) ([]Slot, error) {
	tid, err := s.repo.PublishedTournamentBySlug(ctx, slug)
	if err != nil {
		return nil, err
	}
	return s.repo.ListSlots(ctx, tid)
}

func (s *Service) CreateSlot(ctx context.Context, orgID *uuid.UUID, req CreateSlotRequest) (*Slot, error) {
	tid, err := uuid.Parse(req.TournamentID)
	if err != nil {
		return nil, errors.New("invalid tournament_id")
	}
	if err := s.authorizeTournament(ctx, tid, orgID); err != nil {
		return nil, err
	}
	courtID, err := uuid.Parse(req.CourtID)
	if err != nil {
		return nil, errors.New("invalid court_id")
	}
	var matchID *uuid.UUID
	if req.MatchID != nil && *req.MatchID != "" {
		m, err := uuid.Parse(*req.MatchID)
		if err != nil {
			return nil, errors.New("invalid match_id")
		}
		matchID = &m
	}
	starts, err := time.Parse(time.RFC3339, req.StartsAt)
	if err != nil {
		return nil, errors.New("starts_at must be RFC3339")
	}
	ends, err := time.Parse(time.RFC3339, req.EndsAt)
	if err != nil {
		return nil, errors.New("ends_at must be RFC3339")
	}
	return s.repo.CreateSlot(ctx, tid, courtID, matchID, starts, ends)
}

func (s *Service) UpdateSlot(ctx context.Context, slotID uuid.UUID, orgID *uuid.UUID, req UpdateSlotRequest) (*Slot, error) {
	owner, err := s.repo.SlotTournamentOrg(ctx, slotID)
	if err != nil {
		return nil, err
	}
	if orgID != nil && owner != *orgID {
		return nil, ErrForbidden
	}
	courtID, err := uuid.Parse(req.CourtID)
	if err != nil {
		return nil, errors.New("invalid court_id")
	}
	var matchID *uuid.UUID
	if req.MatchID != nil && *req.MatchID != "" {
		m, err := uuid.Parse(*req.MatchID)
		if err != nil {
			return nil, errors.New("invalid match_id")
		}
		matchID = &m
	}
	starts, err := time.Parse(time.RFC3339, req.StartsAt)
	if err != nil {
		return nil, errors.New("starts_at must be RFC3339")
	}
	ends, err := time.Parse(time.RFC3339, req.EndsAt)
	if err != nil {
		return nil, errors.New("ends_at must be RFC3339")
	}
	return s.repo.UpdateSlot(ctx, slotID, courtID, matchID, starts, ends)
}

func (s *Service) DeleteSlot(ctx context.Context, slotID uuid.UUID, orgID *uuid.UUID) error {
	owner, err := s.repo.SlotTournamentOrg(ctx, slotID)
	if err != nil {
		return err
	}
	if orgID != nil && owner != *orgID {
		return ErrForbidden
	}
	_, err = s.repo.DeleteSlot(ctx, slotID)
	return err
}
