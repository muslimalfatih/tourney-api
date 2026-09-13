-- +goose Up
-- Subphase 5.1 — revocable session foundation.
--
-- Until now sessions were self-contained JWTs: logout was a server-side no-op
-- and a refresh token stayed valid for its full 30 days no matter what happened
-- to the account. Nothing could be withdrawn. This table makes a session a row,
-- so it can be revoked, and gives impersonation (5.5) somewhere to record who
-- is really acting.
--
-- user_id is the EFFECTIVE identity — who the request acts as. During
-- impersonation that is the organizer, while actor_user_id holds the super
-- admin who actually pressed the button, and parent_session_id points back at
-- their own normal session so exiting can restore it.
CREATE TYPE session_kind AS ENUM ('normal', 'impersonation');

CREATE TABLE auth_sessions (
    id                          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id                     UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    actor_user_id               UUID REFERENCES users (id) ON DELETE CASCADE,
    parent_session_id           UUID REFERENCES auth_sessions (id) ON DELETE CASCADE,
    kind                        session_kind NOT NULL DEFAULT 'normal',

    -- Refresh tokens are opaque 32-byte random values, never JWTs. Only the
    -- SHA-256 hash is stored: 256 bits of entropy needs no KDF, unlike a
    -- password. previous_refresh_token_hash keeps exactly one generation of
    -- history so a replayed token is detectable rather than merely unknown.
    refresh_token_hash          TEXT NOT NULL,
    previous_refresh_token_hash TEXT,

    impersonation_reason        TEXT,
    ip_hash                     TEXT,
    user_agent_hash             TEXT,

    created_at                  TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at                TIMESTAMPTZ,
    expires_at                  TIMESTAMPTZ NOT NULL,
    revoked_at                  TIMESTAMPTZ,
    revoked_reason              TEXT,

    -- A normal session has no actor and no parent; an impersonation session
    -- must have both, and can never impersonate its own actor. "Parent must
    -- itself be a normal session" (no nesting) needs to read another row, so a
    -- CHECK cannot express it — the service enforces it and an integration
    -- test covers it.
    CONSTRAINT auth_sessions_shape CHECK (
        (kind = 'normal'
            AND actor_user_id IS NULL
            AND parent_session_id IS NULL)
        OR
        (kind = 'impersonation'
            AND actor_user_id IS NOT NULL
            AND parent_session_id IS NOT NULL
            AND actor_user_id <> user_id)
    )
);

-- Refresh lookup is by hash, so it must be unique and indexed.
CREATE UNIQUE INDEX auth_sessions_refresh_hash_idx
    ON auth_sessions (refresh_token_hash);

-- Replay detection looks here when the current-hash lookup misses.
CREATE INDEX auth_sessions_prev_hash_idx
    ON auth_sessions (previous_refresh_token_hash)
    WHERE previous_refresh_token_hash IS NOT NULL;

-- "Revoke everything for this user" and the per-request session load.
CREATE INDEX auth_sessions_user_active_idx
    ON auth_sessions (user_id) WHERE revoked_at IS NULL;

-- "Revoke every impersonation this super admin started."
CREATE INDEX auth_sessions_actor_active_idx
    ON auth_sessions (actor_user_id)
    WHERE kind = 'impersonation' AND revoked_at IS NULL;

-- "Revoke the children of this session" on sign-out.
CREATE INDEX auth_sessions_parent_idx
    ON auth_sessions (parent_session_id)
    WHERE parent_session_id IS NOT NULL;

-- Same posture as every other table since 00004/00012: the API connects as
-- postgres (BYPASSRLS), so this closes the anon/authenticated keys only.
ALTER TABLE auth_sessions ENABLE ROW LEVEL SECURITY;

-- +goose Down
-- Reversible: this migration only adds. Dropping the table signs everyone out,
-- which is the correct outcome — the access JWTs it issued carry a sid that no
-- longer resolves, so they are rejected rather than silently trusted.
DROP TABLE IF EXISTS auth_sessions;
DROP TYPE IF EXISTS session_kind;
