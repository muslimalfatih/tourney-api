package tournament

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/muslimalfatih/laga-api/internal/audit"
)

// ErrSlugTaken is returned when a chosen slug already exists.
var ErrSlugTaken = errors.New("slug already in use")

// CreateTournamentRequest is the organizer payload for creating a tournament.
// Branding is free-form JSON so white-label config can grow without schema
// churn (see the branding jsonb column). Slug is optional — derived from name.
type CreateTournamentRequest struct {
	Name        string         `json:"name" binding:"required"`
	Slug        string         `json:"slug"`
	Sport       string         `json:"sport" binding:"required,oneof=tennis"`
	Description string         `json:"description"`
	Location    string         `json:"location"`
	StartsOn    string         `json:"starts_on"` // YYYY-MM-DD
	EndsOn      string         `json:"ends_on"`
	Branding    map[string]any `json:"branding"`
}

// UpdateTournamentRequest carries editable fields; all optional (partial update).
type UpdateTournamentRequest struct {
	Name        *string        `json:"name"`
	Description *string        `json:"description"`
	Location    *string        `json:"location"`
	StartsOn    *string        `json:"starts_on"`
	EndsOn      *string        `json:"ends_on"`
	Branding    map[string]any `json:"branding"`
}

// Service holds tournament business logic.
type Service struct {
	repo  *Repository
	audit *audit.Service
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{repo: NewRepository(pool), audit: audit.NewService(pool)}
}

func (s *Service) List(ctx context.Context, orgID uuid.UUID, limit, offset int) ([]Tournament, int64, error) {
	return s.repo.ListByOrg(ctx, orgID, limit, offset)
}

func (s *Service) Get(ctx context.Context, id uuid.UUID, orgID *uuid.UUID) (*Tournament, error) {
	return s.repo.GetByID(ctx, id, orgID)
}

func (s *Service) GetPublic(ctx context.Context, slug string) (*Tournament, error) {
	t, err := s.repo.GetBySlug(ctx, slug)
	if err != nil {
		return nil, err
	}
	// Attach events so the public site can list divisions and default a bracket.
	events, err := s.repo.EventsFor(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	t.Events = events
	return t, nil
}

// Create derives a unique slug if none supplied and persists the tournament.
func (s *Service) Create(ctx context.Context, orgID uuid.UUID, req CreateTournamentRequest) (*Tournament, error) {
	slug := strings.TrimSpace(req.Slug)
	if slug == "" {
		slug = slugify(req.Name)
	} else {
		slug = slugify(slug)
	}

	unique, err := s.ensureUniqueSlug(ctx, slug)
	if err != nil {
		return nil, err
	}

	starts, err := parseDate(req.StartsOn)
	if err != nil {
		return nil, fmt.Errorf("starts_on: %w", err)
	}
	ends, err := parseDate(req.EndsOn)
	if err != nil {
		return nil, fmt.Errorf("ends_on: %w", err)
	}

	in := CreateInput{
		OrgID:       orgID,
		Name:        strings.TrimSpace(req.Name),
		Slug:        unique,
		Sport:       req.Sport,
		Description: strPtr(req.Description),
		Location:    strPtr(req.Location),
		StartsOn:    starts,
		EndsOn:      ends,
		Branding:    req.Branding,
	}
	return s.repo.Create(ctx, in)
}

func (s *Service) Update(ctx context.Context, id uuid.UUID, orgID *uuid.UUID, req UpdateTournamentRequest) (*Tournament, error) {
	in := UpdateInput{Name: req.Name, Description: req.Description, Location: req.Location, Branding: req.Branding}
	if req.StartsOn != nil {
		t, err := parseDate(*req.StartsOn)
		if err != nil {
			return nil, fmt.Errorf("starts_on: %w", err)
		}
		in.StartsOn = t
	}
	if req.EndsOn != nil {
		t, err := parseDate(*req.EndsOn)
		if err != nil {
			return nil, fmt.Errorf("ends_on: %w", err)
		}
		in.EndsOn = t
	}
	return s.repo.Update(ctx, id, orgID, in)
}

func (s *Service) Publish(ctx context.Context, actor uuid.UUID, id uuid.UUID, orgID *uuid.UUID) (*Tournament, error) {
	return s.setStatus(ctx, actor, id, orgID, "published", "tournament.publish")
}

func (s *Service) Unpublish(ctx context.Context, actor uuid.UUID, id uuid.UUID, orgID *uuid.UUID) (*Tournament, error) {
	return s.setStatus(ctx, actor, id, orgID, "draft", "tournament.unpublish")
}

func (s *Service) setStatus(ctx context.Context, actor, id uuid.UUID, orgID *uuid.UUID, status, action string) (*Tournament, error) {
	t, err := s.repo.SetStatus(ctx, id, orgID, status)
	if err != nil {
		return nil, err
	}
	_ = s.audit.Record(ctx, audit.Entry{
		ActorUserID: actor, OrgID: &t.OrgID, TournamentID: &t.ID,
		Action: action, TargetType: "tournament", TargetID: t.ID.String(),
		Diff: map[string]any{"status": t.Status},
	})
	return t, nil
}

// ensureUniqueSlug appends -2, -3, ... until the slug is free.
func (s *Service) ensureUniqueSlug(ctx context.Context, base string) (string, error) {
	candidate := base
	for i := 2; ; i++ {
		exists, err := s.repo.SlugExists(ctx, candidate)
		if err != nil {
			return "", err
		}
		if !exists {
			return candidate, nil
		}
		candidate = fmt.Sprintf("%s-%d", base, i)
		if i > 100 {
			return "", ErrSlugTaken
		}
	}
}

// Helpers -------------------------------------------------------------------

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = nonSlug.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		s = "tournament"
	}
	return s
}

func parseDate(s string) (*time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return nil, errors.New("expected date as YYYY-MM-DD")
	}
	return &t, nil
}

func strPtr(s string) *string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return &s
}
