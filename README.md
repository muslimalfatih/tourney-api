# tourney-api

Backend for [tourney.social](https://tourney.social), a tournament platform for
tennis and padel. One Go modular monolith serves the JSON API, the RBAC auth,
and the live score stream.

This service is the authority on permissions and on every rule about scores,
scheduling and draws. The web app mirrors some of those rules to keep the UI
honest, but it cannot grant anything the API refuses.

- Stack: Go 1.25 with Gin, pgx/v5, sqlc, goose and slog
- Contract: OpenAPI 3.1 in `api/openapi.yaml`
- Realtime: Server-Sent Events, no WebSocket and no Redis
- Database: PostgreSQL, sized for the Supabase free tier

## Requirements

Go 1.25 or newer, from <https://go.dev/dl/>. If you keep several Go versions
installed, put the one you want first on your `PATH`.

A PostgreSQL database. Supabase works, so does local Postgres through the
included `docker-compose.yml`.

Two optional CLIs, only for `make sqlc` and `make migrate-create`. Applying
migrations does not need them, because `make migrate-up` uses the runner
embedded in the binary:

```sh
go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest
go install github.com/pressly/goose/v3/cmd/goose@latest
```

## Quick start

```sh
cp .env.example .env
# Fill in DATABASE_URL, MIGRATION_DATABASE_URL and JWT_SECRET.

# Local Postgres instead of Supabase:
docker compose up -d
# then point both URLs at:
#   postgresql://tourney:tourney@localhost:5432/tourney?sslmode=disable

make migrate-up   # apply the schema
make seed         # create the first super admin from SEED_ADMIN_* in .env
make run          # serve on :8080
```

Check that it came up:

```sh
curl localhost:8080/healthz   # {"status":"ok"}
curl localhost:8080/readyz    # {"status":"ready"} once the database answers
```

Sign in:

```sh
curl -s localhost:8080/api/v1/auth/login \
  -H 'content-type: application/json' \
  -d '{"email":"admin@example.com","password":"change-me-please"}'
```

## Connecting to Supabase

The free tier gives you a small budget of direct connections, so the two URLs
in `.env` point at different endpoints on purpose.

`DATABASE_URL` is the runtime connection and uses the transaction pooler on
port 6543. Keep `DB_MAX_CONNS` small: the limit applies per machine, and a
second machine doubles the real usage.

`MIGRATION_DATABASE_URL` is the migration connection and uses the session
pooler on port 5432, because goose needs session features the transaction
pooler drops.

Both are pgbouncer, which cannot see prepared statements the way a direct
connection can. The pool therefore sets
`DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol`
(`internal/storage/postgres/db.go`). Without it, queries fail with
`prepared statement already exists (SQLSTATE 42P05)`. If you ever point this
service at a direct connection, that line is safe to leave in place.

## Project layout

```
cmd/
  api/       server entrypoint: load config, wire modules, serve
  migrate/   goose runner with the migrations embedded
  seed/      first super admin, demo org, and the Renon Cup demo data
  cleanup/   archives or restores a fixed, fingerprinted list of dev-data rows
internal/
  config/    environment-based configuration
  server/    Gin engine, router, middleware, JSON envelope and errors
  auth/      login, JWT, argon2id, RBAC
  tournament/ event/ participant/ draw/ match/ schedule/ realtime/ audit/ platform/
  storage/   pgxpool and the sqlc-generated queries
  inttest/   integration tests that run against a real database
db/
  migrations/  goose SQL, embedded with //go:embed
  queries/     sqlc sources
api/openapi.yaml
```

## Routes and authorization

Everything sits under `/api/v1`:

| Prefix | Who can call it |
| --- | --- |
| `/api/v1/public/*` | anyone, no token. Read models plus the SSE stream |
| `/api/v1/auth/*`, `/api/v1/me` | anyone, for sign-in and session refresh |
| organizer routes | a token with the `organizer` or `super_admin` role |
| `/api/v1/admin/*` | a token with the `super_admin` role |

Access tokens live 15 minutes and refresh tokens 30 days, both configurable in
`.env`.

## JSON shapes

```jsonc
// one record:  { "data": { ... } }
// a list:      { "data": [ ... ], "meta": { "page", "per_page", "total" } }
// a failure:   { "error": { "code", "message", "details?" } }
```

Validation failures return 422 with a machine-readable `code` and a `details`
object. Conflicts, such as two matches booked on one court at the same time or
a fixture that already exists, return 409 the same way. The web app reads
`code` and shows its own wording, so changing a message string is safe but
changing a code is not.

## Schema

Eleven goose migrations, applied in order. The later ones are worth knowing
about, because each one moves a rule out of application code and into the
database:

- `00004_enable_rls` turns on row-level security
- `00009_scoring` adds per-set scores, and gives walkover, retired and
  cancelled their own match states
- `00010_schedule_conflicts` adds the `btree_gist` exclusion constraint that
  makes a double-booked court impossible rather than merely unlikely
- `00011_tournament_timezone` gives every tournament an IANA timezone.
  Timestamps are stored in UTC and rendered in the tournament's zone

## Make targets

`make help` lists them all. The ones you will use: `run`, `build`,
`migrate-up`, `migrate-down`, `migrate-status`, `migrate-create`, `sqlc`,
`seed`, `fmt`, `vet`, `test` and `test-integration`.

`test-integration` needs a real database in `TEST_DATABASE_URL` or
`DATABASE_URL`, with migrations already applied.

## Deployment

The service runs on Fly.io as the app `tourney-api`, built from the
`Dockerfile` and configured by `fly.toml`. `.github/workflows/fly-deploy.yml`
deploys on push.

Set the secrets before the first deploy, or the release fails:

```sh
fly secrets set DATABASE_URL=... MIGRATION_DATABASE_URL=... JWT_SECRET=... CORS_ORIGINS=...
```

Fly runs `/app/migrate up` in a one-off machine before the new release takes
traffic, so schema changes ship with the code that needs them.

One constraint matters more than the rest. **Deploy with `--ha=false` and keep
the machine count at 1.** The realtime hub holds subscribers in process, so a
second machine would take half the spectators and never tell them about the
scores published on the other one. Live updates would fail quietly, for some
viewers only, which is the hardest kind of bug to notice. Horizontal scaling
needs Redis pub/sub first.

The machine suspends when idle but never stops, because a spectator opening a
live scoreboard should not wait for a cold start.
