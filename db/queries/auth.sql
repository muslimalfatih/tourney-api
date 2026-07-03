-- name: GetUserByEmail :one
SELECT id, email, password_hash, name, role, org_id
FROM users
WHERE email = $1;

-- name: GetUserByID :one
SELECT id, email, password_hash, name, role, org_id
FROM users
WHERE id = $1;

-- name: CreateUser :one
INSERT INTO users (email, password_hash, name, role, org_id)
VALUES ($1, $2, $3, $4, $5)
RETURNING id, email, name, role, org_id, created_at;
