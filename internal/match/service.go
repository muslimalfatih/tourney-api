package match

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/muslimalfatih/tourney-api/internal/draw"
)

// InvalidScoreError carries the structured 422 payload for an illegal score
// submission or scoring-config write.
type InvalidScoreError struct {
	Violations []Violation
}

func (e *InvalidScoreError) Error() string {
	return fmt.Sprintf("invalid score: %d violation(s)", len(e.Violations))
}

var (
	ErrForbidden = errors.New("not permitted")
	// ErrCompletedImmutable guards the status endpoint: reopening a completed
	// match must go through the score-correction transaction, not a bare
	// status flip that would leave stale downstream state unaudited.
	ErrCompletedImmutable = errors.New("completed matches are reopened via a score correction, not a status change")
)

// ScoreRequest is the organizer's score submission. complete=true finalizes the
// match, computes the winner from the sets, and advances the bracket.
// ScoreRequest is a score submission or correction. Completion is one of
// incomplete | normal | walkover | retired | cancelled (see scoring.go).
// WinnerSlot (1|2) is required for walkover/retired — those winners cannot be
// derived — and forbidden otherwise. Sets may be empty only for walkover and
// cancelled.
type ScoreRequest struct {
	Sets       []SetScore `json:"sets"`
	Completion string     `json:"completion" binding:"required"`
	WinnerSlot int        `json:"winner_slot"`
}

// StatusRequest flips scheduling state only. 'completed' is deliberately not
// accepted here — a decided result must come through the score endpoint,
// which validates it and owns progression + audit.
type StatusRequest struct {
	Status string `json:"status" binding:"required,oneof=scheduled live"`
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

// GetPublic is the anonymous read: it returns the match only when its
// tournament is published AND its division is public. Everything else is a
// plain not-found — a draft bracket's match UUIDs must not be probeable.
func (s *Service) GetPublic(ctx context.Context, id uuid.UUID) (*Match, error) {
	_, _, _, published, eventPublic, err := s.repo.EventOrgAndSlug(ctx, id)
	if err != nil {
		return nil, err
	}
	if !published || !eventPublic {
		return nil, ErrNotFound
	}
	return s.repo.Get(ctx, id)
}

// SubmitScore authorizes, computes the winner (when completing), persists, and
// advances the bracket. Returns the refreshed match + the tournament slug for
// SSE broadcast.
// The returned topic is empty when the result must NOT be broadcast on the
// public stream (draft tournament, or hidden division) — the handler skips
// hub.Publish for an empty topic. Persisting still happens; only the public
// broadcast is suppressed.
//
// actor is the authenticated user, recorded on the correction's audit entry.
func (s *Service) SubmitScore(ctx context.Context, matchID, actor uuid.UUID, orgID *uuid.UUID, req ScoreRequest) (*Match, string, error) {
	ownerOrg, tournamentID, slug, published, eventPublic, err := s.repo.EventOrgAndSlug(ctx, matchID)
	if err != nil {
		return nil, "", err
	}
	if orgID != nil && ownerOrg != *orgID {
		return nil, "", ErrForbidden
	}
	topic := ""
	if published && eventPublic {
		topic = slug
	}
	// Validation happens BEFORE the transaction: an illegal submission writes
	// nothing and broadcasts nothing.
	cfg, cfgViolations := ParseScoringConfig(nil) // replaced below when config loads
	raw, err := s.repo.ScoringForMatch(ctx, matchID)
	if err != nil {
		return nil, "", err
	}
	cfg, cfgViolations = ParseScoringConfig(raw)
	if len(cfgViolations) > 0 {
		// A stored config can only be invalid if written before validation
		// existed; fall back to defaults rather than blocking scoring.
		cfg = DefaultScoringConfig()
	}
	winnerSlot, status, violations := ValidateScore(cfg, req.Sets, req.Completion, req.WinnerSlot)
	if len(violations) > 0 {
		return nil, "", &InvalidScoreError{Violations: violations}
	}

	current, err := s.repo.Get(ctx, matchID)
	if err != nil {
		return nil, "", err
	}
	var winnerID *uuid.UUID
	if winnerSlot != 0 {
		winnerID = participantForSlot(current, winnerSlot)
		if winnerID == nil {
			return nil, "", &InvalidScoreError{Violations: []Violation{{
				Field: "winner_slot", Rule: "winner.slot_empty",
				Message: "the winning slot has no participant yet"}}}
		}
	}

	if err := s.repo.SaveScore(ctx, SaveScoreInput{
		MatchID: matchID, Sets: req.Sets, Status: status, WinnerID: winnerID,
		Completion: req.Completion,
		Actor:      actor, OrgID: ownerOrg, TournamentID: tournamentID,
	}); err != nil {
		return nil, "", err
	}

	// If this finished a group-knockout group stage, auto-fill the knockout.
	// Best-effort: a failure here must not fail the score submission.
	if decidedStatus(status) {
		_, _ = s.draw.MaybeResolveGroups(ctx, matchID)
	}

	updated, err := s.repo.Get(ctx, matchID)
	if err != nil {
		return nil, "", err
	}
	return updated, topic, nil
}

// SetStatus authorizes and updates a match's status (e.g. mark live). Topic
// semantics match SubmitScore: empty = do not broadcast publicly.
func (s *Service) SetStatus(ctx context.Context, matchID uuid.UUID, orgID *uuid.UUID, req StatusRequest) (*Match, string, error) {
	ownerOrg, _, slug, published, eventPublic, err := s.repo.EventOrgAndSlug(ctx, matchID)
	if err != nil {
		return nil, "", err
	}
	if orgID != nil && ownerOrg != *orgID {
		return nil, "", ErrForbidden
	}
	topic := ""
	if published && eventPublic {
		topic = slug
	}
	// A completed match cannot be quietly demoted through the status endpoint —
	// that path bypasses the correction transaction (downstream rebuild, group
	// re-resolution, audit). Reopen via a score correction instead, which walks
	// the whole flow.
	current, err := s.repo.Get(ctx, matchID)
	if err != nil {
		return nil, "", err
	}
	if decidedStatus(current.Status) {
		return nil, "", ErrCompletedImmutable
	}
	if err := s.repo.SetStatus(ctx, matchID, req.Status); err != nil {
		return nil, "", err
	}
	m, err := s.repo.Get(ctx, matchID)
	if err != nil {
		return nil, "", err
	}
	return m, topic, nil
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
