package event

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CreateEventRequest is the organizer payload for adding a division.
type CreateEventRequest struct {
	Name       string `json:"name" binding:"required"`
	Discipline string `json:"discipline" binding:"required,oneof=singles doubles"`
	Format     string `json:"format" binding:"required,oneof=single_elim round_robin group_knockout"`
}

type Service struct {
	repo *Repository
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{repo: NewRepository(pool)}
}

// ListForTournament returns a tournament's events after verifying ownership.
func (s *Service) ListForTournament(ctx context.Context, tournamentID uuid.UUID, orgID *uuid.UUID) ([]Event, error) {
	if err := s.repo.tournamentOwned(ctx, tournamentID, orgID); err != nil {
		return nil, err
	}
	return s.repo.ListByTournament(ctx, tournamentID)
}

// ListPublic returns events for a tournament without an org check (public read).
func (s *Service) ListPublic(ctx context.Context, tournamentID uuid.UUID) ([]Event, error) {
	return s.repo.ListByTournament(ctx, tournamentID)
}

func (s *Service) Get(ctx context.Context, id uuid.UUID) (*Event, error) {
	return s.repo.GetByID(ctx, id)
}

func (s *Service) Create(ctx context.Context, tournamentID uuid.UUID, orgID *uuid.UUID, req CreateEventRequest) (*Event, error) {
	if err := s.repo.tournamentOwned(ctx, tournamentID, orgID); err != nil {
		return nil, err
	}
	return s.repo.Create(ctx, CreateInput{
		TournamentID: tournamentID,
		Name:         strings.TrimSpace(req.Name),
		Discipline:   req.Discipline,
		Format:       req.Format,
	})
}

// Delete verifies the event's tournament is owned before removing it.
func (s *Service) Delete(ctx context.Context, id uuid.UUID, orgID *uuid.UUID) error {
	ev, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if err := s.repo.tournamentOwned(ctx, ev.TournamentID, orgID); err != nil {
		return err
	}
	return s.repo.Delete(ctx, id)
}
