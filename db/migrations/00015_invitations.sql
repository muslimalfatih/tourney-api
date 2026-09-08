-- +goose Up
-- Subphase 5.2 — the invitation allowlist.
--
-- Login is invitation-only: an email may request an OTP only if it appears here
-- with an active invitation. There is no public sign-up, so this table is the
-- entire population of people who can ever hold an account.

CREATE TYPE invitation_status AS ENUM ('pending', 'accepted', 'revoked');

CREATE TABLE invitations (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email              TEXT NOT NULL,          -- normalized: lower(trim(email))
    email_display      TEXT,                   -- as the admin typed it
    role               user_role NOT NULL,
    organization_id    UUID REFERENCES organizations (id) ON DELETE CASCADE,
    status             invitation_status NOT NULL DEFAULT 'pending',
    invited_by_user_id UUID REFERENCES users (id) ON DELETE SET NULL,
    note               TEXT,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    accepted_at        TIMESTAMPTZ,
    revoked_at         TIMESTAMPTZ,
    last_sent_at       TIMESTAMPTZ,

    -- Pending invitations expire after 14 days. Accepted ones ignore this: the
    -- account already exists, and an expiry date should not lock anybody out of
    -- an account they have been using.
    expires_at         TIMESTAMPTZ NOT NULL DEFAULT (now() + INTERVAL '14 days'),

    -- Mirrors users_org_scope from 00001, so an invitation can never produce a
    -- user that violates it. Without this an organizer invitation with no
    -- organization would be accepted here and then fail at OTP verify, stranding
    -- someone who did nothing wrong.
    CONSTRAINT invitations_org_scope CHECK (
        (role = 'organizer'   AND organization_id IS NOT NULL) OR
        (role = 'super_admin' AND organization_id IS NULL)
    )
);

-- One active invitation per address, unlimited revoked history. Revoking is
-- terminal: re-inviting inserts a NEW row rather than reviving an old one, so
-- the record of who revoked whom, and when, is never overwritten.
CREATE UNIQUE INDEX invitations_email_active_idx
    ON invitations (email) WHERE status <> 'revoked';

-- The OTP request path looks up by normalized email on every attempt.
CREATE INDEX invitations_email_idx ON invitations (email);

ALTER TABLE invitations ENABLE ROW LEVEL SECURITY;

-- +goose Down
DROP TABLE IF EXISTS invitations;
DROP TYPE IF EXISTS invitation_status;
