-- +goose Up
-- Subphase 5.2 — prepare users for OTP login.
--
-- Three independent changes that all have to land before an invited person can
-- sign in without a password:
--
--   1. status  — suspension needs somewhere to live. Sessions already carry a
--      revocation cascade (00013); this is the flag that triggers it.
--   2. password_hash becomes nullable — an OTP user never has one. Existing
--      hashes are PRESERVED; password login is retired by feature flag first
--      and only dropped in a much later phase, so this stays reversible.
--   3. email becomes case-insensitively unique — the invitation allowlist is
--      keyed by normalized email, so Foo@x.com and foo@x.com must not be two
--      different accounts.

CREATE TYPE user_status AS ENUM ('active', 'suspended');

ALTER TABLE users ADD COLUMN status        user_status NOT NULL DEFAULT 'active';
ALTER TABLE users ADD COLUMN last_login_at TIMESTAMPTZ;

-- OTP users have no password. Nothing is deleted: every existing argon2id hash
-- stays exactly where it is, so flipping AUTH_PASSWORD_LOGIN_ENABLED back on
-- restores password login without a data restore.
ALTER TABLE users ALTER COLUMN password_hash DROP NOT NULL;

-- +goose StatementBegin
-- Refuse to continue if two accounts differ only by case. Merging them would
-- mean choosing whose tournaments survive, which is a product decision and not
-- something a migration may make silently.
DO $$
DECLARE dupes TEXT;
BEGIN
    SELECT string_agg(DISTINCT lower(email), ', ') INTO dupes
    FROM users
    WHERE lower(email) IN (
        SELECT lower(email) FROM users GROUP BY lower(email) HAVING count(*) > 1
    );
    IF dupes IS NOT NULL THEN
        RAISE EXCEPTION
            'case-insensitive email collisions must be resolved by hand first: %', dupes;
    END IF;
END $$;
-- +goose StatementEnd

UPDATE users SET email = lower(trim(email)) WHERE email <> lower(trim(email));

-- CITEXT would need an extension and is not a convention this project uses.
CREATE UNIQUE INDEX users_email_lower_idx ON users (lower(email));

-- +goose Down
DROP INDEX IF EXISTS users_email_lower_idx;
ALTER TABLE users DROP COLUMN IF EXISTS last_login_at;
ALTER TABLE users DROP COLUMN IF EXISTS status;
DROP TYPE IF EXISTS user_status;

-- Deliberately last, and deliberately able to fail: if any OTP-created user has
-- no password, restoring NOT NULL raises rather than inventing one or deleting
-- the row. Set a password for those accounts before rolling back this far.
ALTER TABLE users ALTER COLUMN password_hash SET NOT NULL;
