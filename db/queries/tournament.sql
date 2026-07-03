-- name: GetTournamentBySlug :one
SELECT id, org_id, name, slug, sport, location, starts_on, ends_on,
       branding, status, published_at, created_at
FROM tournaments
WHERE slug = $1;

-- name: ListTournamentsByOrg :many
SELECT id, org_id, name, slug, sport, location, status, created_at
FROM tournaments
WHERE org_id = $1
ORDER BY created_at DESC
LIMIT $2 OFFSET $3;

-- name: CreateTournament :one
INSERT INTO tournaments (org_id, name, slug, sport, location, branding)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, org_id, name, slug, sport, status, created_at;

-- name: SetTournamentStatus :one
UPDATE tournaments
SET status = $2,
    published_at = CASE WHEN $2 = 'published' THEN now() ELSE published_at END
WHERE id = $1
RETURNING id, status, published_at;
