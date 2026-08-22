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
	// OverrideRestBuffer acknowledges rest_buffer warnings from a prior 422 and
	// proceeds anyway. Hard conflicts (court/participant overlap) never yield.
	OverrideRestBuffer bool `json:"override_rest_buffer"`
}

// UpdateSlotRequest edits an existing slot's court, assigned match, and time.
// No tournament_id — the slot is looked up by its own id and re-authorized.
type UpdateSlotRequest struct {
	CourtID            string  `json:"court_id" binding:"required"`
	MatchID            *string `json:"match_id"`
	StartsAt           string  `json:"starts_at" binding:"required"` // RFC3339
	EndsAt             string  `json:"ends_at" binding:"required"`
	OverrideRestBuffer bool    `json:"override_rest_buffer"`
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

// ListPublicSlots returns the schedule for a published tournament by slug,
// scoped to public divisions with a match actually assigned — see
// Repository.ListPublicSlots.
func (s *Service) ListPublicSlots(ctx context.Context, slug string) ([]Slot, error) {
	tid, err := s.repo.PublishedTournamentBySlug(ctx, slug)
	if err != nil {
		return nil, err
	}
	return s.repo.ListPublicSlots(ctx, tid)
}

// parseSlotFields validates the shared court/match/time fields of a slot write.
func parseSlotFields(courtID string, matchID *string, startsAt, endsAt string) (uuid.UUID, *uuid.UUID, time.Time, time.Time, error) {
	cid, err := uuid.Parse(courtID)
	if err != nil {
		return uuid.Nil, nil, time.Time{}, time.Time{}, errors.New("invalid court_id")
	}
	var mid *uuid.UUID
	if matchID != nil && *matchID != "" {
		m, err := uuid.Parse(*matchID)
		if err != nil {
			return uuid.Nil, nil, time.Time{}, time.Time{}, errors.New("invalid match_id")
		}
		mid = &m
	}
	starts, err := time.Parse(time.RFC3339, startsAt)
	if err != nil {
		return uuid.Nil, nil, time.Time{}, time.Time{}, errors.New("starts_at must be RFC3339")
	}
	ends, err := time.Parse(time.RFC3339, endsAt)
	if err != nil {
		return uuid.Nil, nil, time.Time{}, time.Time{}, errors.New("ends_at must be RFC3339")
	}
	if !ends.After(starts) {
		return uuid.Nil, nil, time.Time{}, time.Time{}, errors.New("ends_at must be after starts_at")
	}
	return cid, mid, starts, ends, nil
}

// broadcastSlug returns the tournament's slug if it is published (the SSE
// gate), "" otherwise — mirroring the match handlers' contract.
func (s *Service) broadcastSlug(ctx context.Context, tournamentID uuid.UUID) string {
	slug, published, err := s.repo.TournamentSlugPublished(ctx, tournamentID)
	if err != nil || !published {
		return ""
	}
	return slug
}

func (s *Service) CreateSlot(ctx context.Context, actor uuid.UUID, orgID *uuid.UUID, req CreateSlotRequest) (*Slot, string, error) {
	tid, err := uuid.Parse(req.TournamentID)
	if err != nil {
		return nil, "", errors.New("invalid tournament_id")
	}
	owner, err := s.repo.TournamentOrg(ctx, tid)
	if err != nil {
		return nil, "", err
	}
	if orgID != nil && owner != *orgID {
		return nil, "", ErrForbidden
	}
	courtID, matchID, starts, ends, err := parseSlotFields(req.CourtID, req.MatchID, req.StartsAt, req.EndsAt)
	if err != nil {
		return nil, "", err
	}
	slot, err := s.repo.WriteSlot(ctx, slotWrite{
		tournamentID: tid, courtID: courtID, matchID: matchID,
		startsAt: starts, endsAt: ends,
		overrideRest: req.OverrideRestBuffer, actor: actor, orgID: owner,
	})
	if err != nil {
		return nil, "", err
	}
	return slot, s.broadcastSlug(ctx, tid), nil
}

func (s *Service) UpdateSlot(ctx context.Context, slotID uuid.UUID, actor uuid.UUID, orgID *uuid.UUID, req UpdateSlotRequest) (*Slot, string, error) {
	owner, err := s.repo.SlotTournamentOrg(ctx, slotID)
	if err != nil {
		return nil, "", err
	}
	if orgID != nil && owner != *orgID {
		return nil, "", ErrForbidden
	}
	var tid uuid.UUID
	if err := s.repo.pool.QueryRow(ctx,
		`SELECT tournament_id FROM schedule_slots WHERE id = $1`, slotID).Scan(&tid); err != nil {
		return nil, "", ErrNotFound
	}
	courtID, matchID, starts, ends, err := parseSlotFields(req.CourtID, req.MatchID, req.StartsAt, req.EndsAt)
	if err != nil {
		return nil, "", err
	}
	slot, err := s.repo.WriteSlot(ctx, slotWrite{
		slotID: &slotID, tournamentID: tid, courtID: courtID, matchID: matchID,
		startsAt: starts, endsAt: ends,
		overrideRest: req.OverrideRestBuffer, actor: actor, orgID: owner,
	})
	if err != nil {
		return nil, "", err
	}
	return slot, s.broadcastSlug(ctx, tid), nil
}

func (s *Service) DeleteSlot(ctx context.Context, slotID uuid.UUID, actor uuid.UUID, orgID *uuid.UUID) (string, error) {
	owner, err := s.repo.SlotTournamentOrg(ctx, slotID)
	if err != nil {
		return "", err
	}
	if orgID != nil && owner != *orgID {
		return "", ErrForbidden
	}
	tid, err := s.repo.DeleteSlot(ctx, slotID, actor, owner)
	if err != nil {
		return "", err
	}
	return s.broadcastSlug(ctx, tid), nil
}
