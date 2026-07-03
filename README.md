# laga-api

Backend for **Laga**, a white-label tennis tournament platform. Go modular
monolith: HTTP JSON API, RBAC auth, and SSE live updates in one codebase.

- **Stack:** Go · Gin · pgx/v5 · sqlc · goose · slog
- **Contract:** OpenAPI 3.1 (`api/openapi.yaml`) is the source of truth
- **Realtime:** Server-Sent Events (no WebSocket, no Redis)
- **DB:** PostgreSQL (Supabase Free-compatible)

## Requirements

- **Go 1.24+** (this repo pins a recent toolchain). Install from
  <https://go.dev/dl/>. If you keep multiple Go versions, put the desired
  `go/bin` first on your `PATH`.
- A PostgreSQL database — Supabase Free, or local Postgres via Docker.
- Optional CLIs: [`sqlc`](https://sqlc.dev), [`goose`](https://github.com/pressly/goose)
  (only needed for `make sqlc` / `make migrate-create`; `make migrate-up` uses
  the embedded runner and needs neither).

```sh
go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest
go install github.com/pressly/goose/v3/cmd/goose@latest
```

## Quick start

```sh
cp .env.example .env
# Edit .env: set DATABASE_URL / MIGRATION_DATABASE_URL and JWT_SECRET.

# Local Postgres (alternative to Supabase):
docker compose up -d
# then in .env point both URLs at:
#   postgresql://laga:laga@localhost:5432/laga?sslmode=disable

make migrate-up      # apply schema
make seed            # create the initial super admin (SEED_ADMIN_* in .env)
make run             # start the API on :8080
```

Health check:

```sh
curl localhost:8080/healthz   # {"status":"ok"}
curl localhost:8080/readyz    # {"status":"ready"} once the DB is reachable
```

Log in:

```sh
curl -s localhost:8080/api/v1/auth/login \
  -H 'content-type: application/json' \
  -d '{"email":"admin@example.com","password":"change-me-please"}'
```

## Supabase Free notes

The free tier has a small direct-connection budget, so:

- **Runtime** (`DATABASE_URL`) uses the **transaction pooler** endpoint
  (`...pooler.supabase.com:6543`). The pool is capped small (`DB_MAX_CONNS`).
- **Migrations** (`MIGRATION_DATABASE_URL`) use a **direct** connection
  (port 5432) because goose needs session-level features the pooler lacks.

No Supabase Pro features are required. No Redis/queue is used.

## Project layout

```
cmd/
  api/       API server entrypoint (thin: config → wire → serve)
  migrate/   embedded goose migration runner
  seed/      create the initial super admin
internal/
  config/    env-based configuration
  server/    Gin engine, router, middleware, JSON envelope + errors
  auth/      login, JWT, argon2id, RBAC
  tournament/ event/ participant/ draw/ match/ schedule/ realtime/ audit/ platform/
  storage/   pgxpool + sqlc-generated queries
db/
  migrations/  goose SQL (embedded via //go:embed)
  queries/     sqlc query sources
api/openapi.yaml  the contract
```

Routes are grouped under `/api/v1`:

- `/api/v1/public/*` — unauthenticated read models + SSE (consumed by web SSR)
- `/api/v1/auth/*`, `/api/v1/me` — authentication
- organizer routes — require `organizer` or `super_admin`
- `/api/v1/admin/*` — require `super_admin`

**Authorization is enforced here, in the API.** The web app's role checks are
UX only.

## JSON conventions

```jsonc
// single:  { "data": { ... } }
// list:    { "data": [ ... ], "meta": { "page", "per_page", "total" } }
// error:   { "error": { "code", "message", "details?" } }
```

## Make targets

Run `make help` for the full list. Common ones: `run`, `build`, `migrate-up`,
`migrate-down`, `migrate-status`, `sqlc`, `seed`, `vet`, `test`.

## Status

Milestone 0 skeleton: config, server, health, auth (fully working), RBAC, SSE
hub, full schema, and wired module stubs returning typed shapes. Business logic
for tournaments/events/participants/draws/matches/schedule lands in milestones
1–4 (see the project plan).
```
