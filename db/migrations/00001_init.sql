-- +goose Up
-- +goose StatementBegin

-- Extensions -----------------------------------------------------------------
CREATE EXTENSION IF NOT EXISTS "pgcrypto"; -- gen_random_uuid()

-- Enums ----------------------------------------------------------------------
CREATE TYPE user_role         AS ENUM ('super_admin', 'organizer');
CREATE TYPE org_status        AS ENUM ('active', 'suspended');
CREATE TYPE tournament_status AS ENUM ('draft', 'published', 'archived', 'suspended');
CREATE TYPE event_discipline  AS ENUM ('singles', 'doubles');
CREATE TYPE event_format      AS ENUM ('single_elim', 'round_robin', 'group_knockout');
CREATE TYPE stage_kind        AS ENUM ('group', 'knockout');
CREATE TYPE match_status      AS ENUM ('pending', 'scheduled', 'live', 'completed', 'walkover', 'bye');

-- Organizations & users ------------------------------------------------------
CREATE TABLE organizations (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT        NOT NULL,
    slug        TEXT        NOT NULL UNIQUE,
    branding    JSONB       NOT NULL DEFAULT '{}'::jsonb,
    status      org_status  NOT NULL DEFAULT 'active',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE users (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email         TEXT        NOT NULL UNIQUE,
    password_hash TEXT        NOT NULL,
    name          TEXT        NOT NULL,
    role          user_role   NOT NULL,
    org_id        UUID        REFERENCES organizations (id) ON DELETE CASCADE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Organizers must belong to an org; super admins must not.
    CONSTRAINT users_org_scope CHECK (
        (role = 'organizer'   AND org_id IS NOT NULL) OR
        (role = 'super_admin' AND org_id IS NULL)
    )
);
CREATE INDEX idx_users_org ON users (org_id);

-- Tournaments ----------------------------------------------------------------
CREATE TABLE tournaments (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id       UUID              NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    name         TEXT              NOT NULL,
    slug         TEXT              NOT NULL UNIQUE,
    sport        TEXT              NOT NULL DEFAULT 'tennis',
    location     TEXT,
    starts_on    DATE,
    ends_on      DATE,
    branding     JSONB             NOT NULL DEFAULT '{}'::jsonb,
    status       tournament_status NOT NULL DEFAULT 'draft',
    published_at TIMESTAMPTZ,
    created_at   TIMESTAMPTZ       NOT NULL DEFAULT now()
);
CREATE INDEX idx_tournaments_org    ON tournaments (org_id);
CREATE INDEX idx_tournaments_status ON tournaments (status);

-- Venues & courts ------------------------------------------------------------
CREATE TABLE venues (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tournament_id UUID NOT NULL REFERENCES tournaments (id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    address       TEXT
);

CREATE TABLE courts (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    venue_id   UUID    NOT NULL REFERENCES venues (id) ON DELETE CASCADE,
    name       TEXT    NOT NULL,
    sort_order INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_courts_venue ON courts (venue_id);

-- Scoring profiles -----------------------------------------------------------
-- config JSONB holds sport-specific rules (best-of, tiebreak, no-ad). Keeping
-- rules as JSON makes tennis configurable now and padel possible later with no
-- schema change.
CREATE TABLE scoring_profiles (
    id     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name   TEXT  NOT NULL,
    sport  TEXT  NOT NULL DEFAULT 'tennis',
    config JSONB NOT NULL DEFAULT '{}'::jsonb
);

-- Events (divisions), stages, groups -----------------------------------------
CREATE TABLE events (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tournament_id      UUID             NOT NULL REFERENCES tournaments (id) ON DELETE CASCADE,
    name               TEXT             NOT NULL,
    discipline         event_discipline NOT NULL DEFAULT 'singles',
    format             event_format     NOT NULL,
    scoring_profile_id UUID             REFERENCES scoring_profiles (id),
    created_at         TIMESTAMPTZ      NOT NULL DEFAULT now()
);
CREATE INDEX idx_events_tournament ON events (tournament_id);

CREATE TABLE stages (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id   UUID       NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    kind       stage_kind NOT NULL,
    sort_order INTEGER    NOT NULL DEFAULT 0
);
CREATE INDEX idx_stages_event ON stages (event_id);

CREATE TABLE groups (
    id       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    stage_id UUID NOT NULL REFERENCES stages (id) ON DELETE CASCADE,
    name     TEXT NOT NULL
);
CREATE INDEX idx_groups_stage ON groups (stage_id);

-- Players, teams, participants -----------------------------------------------
CREATE TABLE players (
    id     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id UUID  NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    name   TEXT  NOT NULL,
    gender TEXT,
    meta   JSONB NOT NULL DEFAULT '{}'::jsonb
);
CREATE INDEX idx_players_org ON players (org_id);

CREATE TABLE teams (
    id       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id UUID NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    name     TEXT
);

-- A participant is the unifying "entry" in an event: singles → a player,
-- doubles → a team.
CREATE TABLE participants (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id     UUID    NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    team_id      UUID    REFERENCES teams (id)   ON DELETE CASCADE,
    player_id    UUID    REFERENCES players (id) ON DELETE CASCADE,
    display_name TEXT    NOT NULL,
    seed         INTEGER,
    CONSTRAINT participants_entry CHECK (team_id IS NOT NULL OR player_id IS NOT NULL)
);
CREATE INDEX idx_participants_event ON participants (event_id);

CREATE TABLE registrations (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id       UUID        NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    participant_id UUID        NOT NULL REFERENCES participants (id) ON DELETE CASCADE,
    status         TEXT        NOT NULL DEFAULT 'confirmed',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE seedings (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id       UUID    NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    participant_id UUID    NOT NULL REFERENCES participants (id) ON DELETE CASCADE,
    seed           INTEGER NOT NULL,
    UNIQUE (event_id, seed)
);

-- Rounds & matches -----------------------------------------------------------
CREATE TABLE rounds (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    stage_id     UUID    NOT NULL REFERENCES stages (id) ON DELETE CASCADE,
    round_number INTEGER NOT NULL,
    name         TEXT    NOT NULL
);
CREATE INDEX idx_rounds_stage ON rounds (stage_id);

CREATE TABLE matches (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id              UUID         NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    stage_id              UUID         REFERENCES stages (id) ON DELETE CASCADE,
    group_id              UUID         REFERENCES groups (id) ON DELETE CASCADE,
    round_id              UUID         REFERENCES rounds (id) ON DELETE CASCADE,
    court_id              UUID         REFERENCES courts (id) ON DELETE SET NULL,
    match_no              INTEGER      NOT NULL DEFAULT 0,
    status                match_status NOT NULL DEFAULT 'pending',
    scheduled_at          TIMESTAMPTZ,
    started_at            TIMESTAMPTZ,
    completed_at          TIMESTAMPTZ,
    winner_participant_id UUID         REFERENCES participants (id) ON DELETE SET NULL,
    -- Bracket progression: winner of this match feeds next_match_id at next_slot.
    next_match_id         UUID         REFERENCES matches (id) ON DELETE SET NULL,
    next_slot             SMALLINT
);
CREATE INDEX idx_matches_event ON matches (event_id);
CREATE INDEX idx_matches_round ON matches (round_id);
CREATE INDEX idx_matches_next  ON matches (next_match_id);

CREATE TABLE match_participants (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    match_id       UUID     NOT NULL REFERENCES matches (id) ON DELETE CASCADE,
    participant_id UUID     REFERENCES participants (id) ON DELETE CASCADE,
    slot           SMALLINT NOT NULL, -- 1 or 2
    -- For unresolved slots: describes where the entrant comes from
    -- (e.g. {"winner_of_match": "..."}).
    source         JSONB,
    UNIQUE (match_id, slot)
);
CREATE INDEX idx_match_participants_match ON match_participants (match_id);

-- Tennis-shaped set scores. One row per set.
CREATE TABLE match_scores (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    match_id    UUID     NOT NULL REFERENCES matches (id) ON DELETE CASCADE,
    set_number  SMALLINT NOT NULL,
    p1_games    SMALLINT NOT NULL DEFAULT 0,
    p2_games    SMALLINT NOT NULL DEFAULT 0,
    p1_tiebreak SMALLINT,
    p2_tiebreak SMALLINT,
    UNIQUE (match_id, set_number)
);

-- Schedule -------------------------------------------------------------------
CREATE TABLE schedule_slots (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tournament_id UUID        NOT NULL REFERENCES tournaments (id) ON DELETE CASCADE,
    court_id      UUID        NOT NULL REFERENCES courts (id) ON DELETE CASCADE,
    match_id      UUID        REFERENCES matches (id) ON DELETE SET NULL,
    starts_at     TIMESTAMPTZ NOT NULL,
    ends_at       TIMESTAMPTZ NOT NULL
);
CREATE INDEX idx_schedule_slots_tournament ON schedule_slots (tournament_id);
CREATE INDEX idx_schedule_slots_court      ON schedule_slots (court_id);

-- Audit log ------------------------------------------------------------------
CREATE TABLE audit_logs (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id        UUID        REFERENCES organizations (id) ON DELETE SET NULL,
    actor_user_id UUID        REFERENCES users (id) ON DELETE SET NULL,
    tournament_id UUID        REFERENCES tournaments (id) ON DELETE SET NULL,
    action        TEXT        NOT NULL,
    target_type   TEXT        NOT NULL,
    target_id     TEXT        NOT NULL,
    diff          JSONB,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_audit_logs_tournament ON audit_logs (tournament_id);
CREATE INDEX idx_audit_logs_org        ON audit_logs (org_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS audit_logs;
DROP TABLE IF EXISTS schedule_slots;
DROP TABLE IF EXISTS match_scores;
DROP TABLE IF EXISTS match_participants;
DROP TABLE IF EXISTS matches;
DROP TABLE IF EXISTS rounds;
DROP TABLE IF EXISTS seedings;
DROP TABLE IF EXISTS registrations;
DROP TABLE IF EXISTS participants;
DROP TABLE IF EXISTS teams;
DROP TABLE IF EXISTS players;
DROP TABLE IF EXISTS groups;
DROP TABLE IF EXISTS stages;
DROP TABLE IF EXISTS events;
DROP TABLE IF EXISTS scoring_profiles;
DROP TABLE IF EXISTS courts;
DROP TABLE IF EXISTS venues;
DROP TABLE IF EXISTS tournaments;
DROP TABLE IF EXISTS users;
DROP TABLE IF EXISTS organizations;
DROP TYPE IF EXISTS match_status;
DROP TYPE IF EXISTS stage_kind;
DROP TYPE IF EXISTS event_format;
DROP TYPE IF EXISTS event_discipline;
DROP TYPE IF EXISTS tournament_status;
DROP TYPE IF EXISTS org_status;
DROP TYPE IF EXISTS user_role;
-- +goose StatementEnd
