package tournament

import "github.com/jackc/pgx/v5/pgxpool"

// CreateTournamentRequest is the organizer payload for creating a tournament.
// Branding is free-form JSON so white-label config can grow without schema
// churn (see the branding jsonb column).
type CreateTournamentRequest struct {
	Name     string         `json:"name" binding:"required"`
	Slug     string         `json:"slug" binding:"required"`
	Sport    string         `json:"sport" binding:"required,oneof=tennis"`
	Location string         `json:"location"`
	Branding map[string]any `json:"branding"`
}

// Service holds tournament business logic. Bodies land in milestone 1; the
// pool is wired now so the dependency graph and constructor signatures are
// stable.
type Service struct {
	pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}
