package event

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/muslimalfatih/laga-api/internal/match"
)

// CreateEventRequest is the organizer payload for adding a division.
type CreateEventRequest struct {
	Name       string `json:"name" binding:"required"`
	Discipline string `json:"discipline" binding:"required,oneof=singles doubles"`
	Format     string `json:"format" binding:"required,oneof=single_elim round_robin group_knockout"`
	// Optional, and needed at create time rather than in the follow-up patch:
	// the slug is qualified with the category when a plain name slug is already
	// taken ("mens-singles-intermediate"), and the slug is only assigned once.
	Category string `json:"category"`
}

// UpdateEventRequest patches the public-facing event config. Every field is a
// pointer so an omitted (or null) key leaves the column untouched; a present
// value is applied. For category/public_display_name a present "" clears the
// column to NULL (uncategorised / falls back to name).
type UpdateEventRequest struct {
	Category          *string         `json:"category"`
	Gender            *string         `json:"gender" binding:"omitempty,oneof=men women mixed"`
	IsPublic          *bool           `json:"is_public"`
	PublicDisplayName *string         `json:"public_display_name"`
	PublicOrder       *int            `json:"public_order"`
	Scoring           json.RawMessage `json:"scoring"`
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
	name := strings.TrimSpace(req.Name)

	// The slug is assigned once, here, and never recomputed — a later rename
	// leaves it alone so links already shared keep resolving.
	//
	// Read-then-write is racy in principle: two divisions created in the same
	// millisecond could pick the same slug. The unique index on
	// (tournament_id, slug) is what actually guarantees correctness; this loop
	// just makes the common path produce a nice slug. On the (vanishingly rare)
	// losing write we retry, which re-reads the now-updated set.
	// ponytail: retry loop over a unique-violation, not an advisory lock —
	// swap it for a lock if divisions ever get created in bulk concurrently.
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		existing, err := s.repo.ListByTournament(ctx, tournamentID)
		if err != nil {
			return nil, err
		}
		taken := make(map[string]bool, len(existing))
		for _, e := range existing {
			taken[e.Slug] = true
		}

		ev, err := s.repo.Create(ctx, CreateInput{
			TournamentID: tournamentID,
			Name:         name,
			Slug:         buildSlug(name, strings.TrimSpace(req.Category), taken),
			Discipline:   req.Discipline,
			Format:       req.Format,
			Category:     strings.TrimSpace(req.Category),
		})
		if err == nil {
			return ev, nil
		}
		if !isUniqueViolation(err) {
			return nil, err
		}
		lastErr = err
	}
	return nil, lastErr
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
	// Scoring config is validated here — the one write path — so an invalid
	// document can never be stored (match.ValidateScore still defends in
	// depth with a default fallback for pre-validation rows).
	if in.Scoring != nil {
		if _, violations := match.ParseScoringConfig([]byte(*in.Scoring)); len(violations) > 0 {
			return nil, &match.InvalidScoreError{Violations: violations}
		}
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
