# laga-api container image for Fly.io.
#
# Two binaries ship in one image: the API server (entrypoint) and the goose
# migrator, which fly.toml runs as a release_command before each deploy goes
# live. Migrations need MIGRATION_DATABASE_URL (Supabase session pooler, port
# 5432 + simple_protocol); the server itself uses DATABASE_URL (transaction
# pooler, 6543).

# ---- build ----------------------------------------------------------------
FROM golang:1.25-alpine AS build
WORKDIR /src

# Dependencies first so a code-only change reuses this layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# CGO off => a static binary that runs on the tiny runtime image below.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/migrate ./cmd/migrate

# ---- runtime ---------------------------------------------------------------
FROM alpine:3.21
# ca-certificates: TLS to Supabase. tzdata: tournament timezones (Phase 3.6)
# resolve through time.LoadLocation, which reads the system zone database.
RUN apk add --no-cache ca-certificates tzdata && \
    adduser -D -u 10001 laga
USER laga
WORKDIR /app

COPY --from=build /out/api /app/api
COPY --from=build /out/migrate /app/migrate

ENV PORT=8080
EXPOSE 8080
CMD ["/app/api"]
