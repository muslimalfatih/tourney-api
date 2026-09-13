package platform

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/muslimalfatih/tourney-api/internal/audit"
	"github.com/muslimalfatih/tourney-api/internal/auth"
)

// CreateOrgRequest creates an organization and its first organizer account.
type CreateOrgRequest struct {
	OrgName        string `json:"org_name" binding:"required"`
	Slug           string `json:"slug"`
	OrganizerEmail string `json:"organizer_email" binding:"required,email"`
	OrganizerName  string `json:"organizer_name" binding:"required"`
	Password       string `json:"password" binding:"required,min=8"`
}

type SuspendRequest struct {
	Reason string `json:"reason"`
	// action: suspend | archive | restore
	Action string `json:"action" binding:"required,oneof=publish unpublish suspend archive restore"`
}

type Service struct {
	pool        *pgxpool.Pool
	repo        *Repository
	audit       *audit.Service
	sessions    *auth.SessionRepository
	invitations *auth.InvitationRepository
	authSvc     *auth.Service
}

// Deps is everything the platform surface needs from the auth domain. Wired
// explicitly in main.go like every other module.
type Deps struct {
	Pool        *pgxpool.Pool
	Audit       *audit.Service
	Sessions    *auth.SessionRepository
	Invitations *auth.InvitationRepository
	Auth        *auth.Service
}

func NewService(d Deps) *Service {
	return &Service{
		pool: d.Pool, repo: NewRepository(d.Pool), audit: d.Audit,
		sessions: d.Sessions, invitations: d.Invitations, authSvc: d.Auth,
	}
}

func (s *Service) ListOrgs(ctx context.Context, limit, offset int) ([]Organization, int64, error) {
	return s.repo.ListOrgs(ctx, limit, offset)
}

func (s *Service) CreateOrg(ctx context.Context, actor uuid.UUID, req CreateOrgRequest) (*Organization, error) {
	slug := slugify(req.Slug)
	if slug == "" {
		slug = slugify(req.OrgName)
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		return nil, err
	}
	org, err := s.repo.CreateOrgWithOrganizer(ctx, strings.TrimSpace(req.OrgName), slug,
		strings.ToLower(strings.TrimSpace(req.OrganizerEmail)), strings.TrimSpace(req.OrganizerName), hash)
	if err != nil {
		return nil, err
	}
	_ = s.audit.Record(ctx, audit.Entry{
		ActorUserID: actor, OrgID: &org.ID, Action: "org.create",
		TargetType: "organization", TargetID: org.ID.String(),
		Diff: map[string]any{"name": org.Name, "organizer_email": req.OrganizerEmail},
	})
	return org, nil
}

func (s *Service) ListAllTournaments(ctx context.Context, limit, offset int) ([]GlobalTournament, int64, error) {
	return s.repo.ListAllTournaments(ctx, limit, offset)
}

// Force-action audit names, per the platform spec. "suspend" keeps its
// original name for continuity with rows already written.
const (
	ActionTournamentForcePublished   = "admin.tournament_force_published"
	ActionTournamentForceUnpublished = "admin.tournament_force_unpublished"
	ActionTournamentArchived         = "admin.tournament_archived"
	ActionTournamentRestored         = "admin.tournament_restored"
	ActionTournamentSuspended        = "tournament.suspend"
)

// ErrReasonRequired: a force action without a stated reason is a force action
// nobody can later explain.
var ErrReasonRequired = errors.New("a reason is required for this action")

// SetTournamentStatus applies a platform oversight action.
//
// publish / unpublish / archive / restore / suspend all route through the
// tournament's status column, which is the same gate the public read path,
// the OG-metadata injector and the SSE stream check — so a force unpublish
// closes all three at once with no further work. A test proves it.
func (s *Service) SetTournamentStatus(ctx context.Context, by Attribution, id uuid.UUID, action string, reason *string) (*GlobalTournament, error) {
	if reason == nil || strings.TrimSpace(*reason) == "" {
		return nil, ErrReasonRequired
	}
	var status, auditAction string
	switch action {
	case "publish":
		status, auditAction = "published", ActionTournamentForcePublished
	case "unpublish":
		status, auditAction = "draft", ActionTournamentForceUnpublished
	case "archive":
		status, auditAction = "archived", ActionTournamentArchived
	case "restore":
		status, auditAction = "draft", ActionTournamentRestored
	case "suspend":
		status, auditAction = "suspended", ActionTournamentSuspended
	default:
		return nil, errors.New("unknown action")
	}
	before, err := s.repo.GetTournamentStatus(ctx, id)
	if err != nil {
		return nil, err
	}
	t, err := s.repo.SetTournamentStatus(ctx, id, status)
	if err != nil {
		return nil, err
	}
	_ = s.audit.Record(ctx, by.entry(audit.Entry{
		OrgID: &t.OrgID, TournamentID: &t.ID, Reason: reason,
		Action: auditAction, TargetType: "tournament", TargetID: t.ID.String(),
		Diff: map[string]any{"status": map[string]string{"from": before, "to": t.Status}},
	}))
	return t, nil
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = nonSlug.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}
