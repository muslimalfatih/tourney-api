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

// UpdateEventRequest patches the public-facing event config. Every field is a
// pointer so an omitted (or null) key leaves the column untouched; a present
// value is applied. For category/public_display_name a present "" clears the
// column to NULL (uncategorised / falls back to name).
type UpdateEventRequest struct {
	Category          *string `json:"category"`
	Gender            *string `json:"gender" binding:"omitempty,oneof=men women mixed"`
	IsPublic          *bool   `json:"is_public"`
	PublicDisplayName *string `json:"public_display_name"`
	PublicOrder       *int    `json:"public_order"`
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

// AuthorizeEvent verifies the caller's org owns the event's tournament (nil =
// super admin). Returns ErrNotFound if the event isn't visible to the caller,
// so organizer read-model routes can't leak across orgs. Used by the ungated
// organizer bracket/standings/groups reads.
func (s *Service) AuthorizeEvent(ctx context.Context, id uuid.UUID, orgID *uuid.UUID) error {
	ev, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return err
	}
	return s.repo.tournamentOwned(ctx, ev.TournamentID, orgID)
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

// UpdatePublicSettings verifies the event's tournament is owned, then patches
// its public-facing config. Trimming + empty→NULL is handled in SQL (NULLIF/
// btrim), so the request pointers pass straight through — preserving the
// present-"" vs absent distinction the repo relies on.
func (s *Service) UpdatePublicSettings(ctx context.Context, id uuid.UUID, orgID *uuid.UUID, in UpdateInput) (*Event, error) {
	ev, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.repo.tournamentOwned(ctx, ev.TournamentID, orgID); err != nil {
		return nil, err
	}
	return s.repo.Update(ctx, id, in)
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
