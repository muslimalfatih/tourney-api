package participant

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrForbidden = errors.New("not permitted")

// AddParticipantRequest adds an entry to an event. For singles, DisplayName is
// the player's name; for doubles, it's the pairing label (e.g. "Wibowo / Sari").
type AddParticipantRequest struct {
	DisplayName string `json:"display_name" binding:"required"`
	Seed        *int   `json:"seed"`
}

type SetSeedRequest struct {
	Seed *int `json:"seed"`
}

type Service struct {
	repo *Repository
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{repo: NewRepository(pool)}
}

// authorizeEvent checks the caller's org owns the event's tournament and
// returns the event discipline. orgID nil = super admin (allowed).
func (s *Service) authorizeEvent(ctx context.Context, eventID uuid.UUID, orgID *uuid.UUID) (string, error) {
	ownerOrg, discipline, err := s.repo.eventOrg(ctx, eventID)
	if err != nil {
		return "", err
	}
	if orgID != nil && ownerOrg != *orgID {
		return "", ErrForbidden
	}
	return discipline, nil
}

func (s *Service) List(ctx context.Context, eventID uuid.UUID, orgID *uuid.UUID) ([]Participant, error) {
	if _, err := s.authorizeEvent(ctx, eventID, orgID); err != nil {
		return nil, err
	}
	return s.repo.ListByEvent(ctx, eventID)
}

// ListPublic returns an event's roster only if its tournament is published.
func (s *Service) ListPublic(ctx context.Context, eventID uuid.UUID) ([]Participant, error) {
	published, err := s.repo.EventTournamentPublished(ctx, eventID)
	if err != nil {
		return nil, err
	}
	if !published {
		return nil, ErrNotFound
	}
	return s.repo.ListByEvent(ctx, eventID)
}

// Add creates the right kind of entry based on the event's discipline.
func (s *Service) Add(ctx context.Context, eventID uuid.UUID, orgID *uuid.UUID, req AddParticipantRequest) (*Participant, error) {
	discipline, err := s.authorizeEvent(ctx, eventID, orgID)
	if err != nil {
		return nil, err
	}
	ownerOrg, _, _ := s.repo.eventOrg(ctx, eventID)
	if discipline == "doubles" {
		return s.repo.CreateDoubles(ctx, eventID, req.DisplayName, req.Seed)
	}
	return s.repo.CreateSingles(ctx, ownerOrg, eventID, req.DisplayName, req.Seed)
}

func (s *Service) SetSeed(ctx context.Context, id uuid.UUID, orgID *uuid.UUID, req SetSeedRequest) (*Participant, error) {
	if err := s.authorizeParticipant(ctx, id, orgID); err != nil {
		return nil, err
	}
	return s.repo.SetSeed(ctx, id, req.Seed)
}

func (s *Service) Delete(ctx context.Context, id uuid.UUID, orgID *uuid.UUID) error {
	if err := s.authorizeParticipant(ctx, id, orgID); err != nil {
		return err
	}
	_, err := s.repo.Delete(ctx, id)
	return err
}

func (s *Service) authorizeParticipant(ctx context.Context, id uuid.UUID, orgID *uuid.UUID) error {
	ownerOrg, err := s.repo.EventOrgFor(ctx, id)
	if err != nil {
		return err
	}
	if orgID != nil && ownerOrg != *orgID {
		return ErrForbidden
	}
	return nil
}
