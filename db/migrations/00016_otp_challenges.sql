-- +goose Up
-- Subphase 5.3 — one-time sign-in codes.
--
-- A challenge is created when an invited address asks to sign in, and dies in
-- one of four ways: consumed (used successfully), invalidated (superseded by a
-- newer request, or locked after too many wrong guesses), expired, or simply
-- never touched again.
--
-- The code itself is NEVER stored. code_hash is HMAC-SHA256 over
-- "email:code" keyed by OTP_PEPPER, so a database reader cannot brute-force
-- the 10^6 keyspace offline without also holding the pepper, and a code minted
-- for one address cannot verify another.
CREATE TABLE otp_challenges (
    id                      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email                   TEXT NOT NULL,              -- normalized lowercase
    purpose                 TEXT NOT NULL DEFAULT 'login',
    code_hash               TEXT NOT NULL,
    invitation_id           UUID REFERENCES invitations (id) ON DELETE SET NULL,

    attempts                INT NOT NULL DEFAULT 0,
    max_attempts            INT NOT NULL DEFAULT 5,

    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at              TIMESTAMPTZ NOT NULL,
    consumed_at             TIMESTAMPTZ,
    invalidated_at          TIMESTAMPTZ,
    last_attempt_at         TIMESTAMPTZ,

    -- Hashed, never raw: these exist to correlate abuse, not to identify
    -- people, and an audit trail should not become a location history.
    requested_ip_hash       TEXT,
    request_user_agent_hash TEXT
);

-- The verify path looks up exactly one row: the newest live challenge for an
-- address. Partial index so dead challenges do not bloat it.
CREATE INDEX otp_challenges_live_idx
    ON otp_challenges (email, purpose, created_at DESC)
    WHERE consumed_at IS NULL AND invalidated_at IS NULL;

-- Requesting a new code invalidates prior live ones for the same address.
CREATE INDEX otp_challenges_email_idx ON otp_challenges (email, purpose);

ALTER TABLE otp_challenges ENABLE ROW LEVEL SECURITY;

-- +goose Down
DROP TABLE IF EXISTS otp_challenges;
