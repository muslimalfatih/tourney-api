package platform

import (
	"context"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/muslimalfatih/laga-api/internal/audit"
	"github.com/muslimalfatih/laga-api/internal/auth"
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
	// action: suspend | archive | restore
	Action string `json:"action" binding:"required,oneof=suspend archive restore"`
}

type Service struct {
	repo  *Repository
	audit *audit.Service
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{repo: NewRepository(pool), audit: audit.NewService(pool)}
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

// SetTournamentStatus maps an oversight action to a status transition and
// records it in the audit log.
func (s *Service) SetTournamentStatus(ctx context.Context, actor uuid.UUID, id uuid.UUID, action string) (*GlobalTournament, error) {
	var status string
	switch action {
	case "suspend":
		status = "suspended"
	case "archive":
		status = "archived"
	case "restore":
		status = "draft"
	default:
		status = "draft"
	}
	t, err := s.repo.SetTournamentStatus(ctx, id, status)
	if err != nil {
		return nil, err
	}
	_ = s.audit.Record(ctx, audit.Entry{
		ActorUserID: actor, OrgID: &t.OrgID, TournamentID: &t.ID,
		Action: "tournament." + action, TargetType: "tournament", TargetID: t.ID.String(),
		Diff: map[string]any{"status": t.Status},
	})
	return t, nil
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = nonSlug.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}
