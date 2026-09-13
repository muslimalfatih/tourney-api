-- +goose Up
-- Subphase 5.5 — audit rows learn to tell two people apart.
--
-- Until now an audit row had one actor. During impersonation there are two: the
-- super admin who actually pressed the button, and the organizer whose identity
-- the request ran under. Both matter. actor_user_id keeps its meaning (the real
-- human); effective_user_id is the borrowed identity, NULL whenever nobody is
-- impersonating, so every existing row and every existing query keeps working.
ALTER TABLE audit_logs ADD COLUMN effective_user_id UUID
    REFERENCES users (id) ON DELETE SET NULL;
ALTER TABLE audit_logs ADD COLUMN impersonation_session_id UUID
    REFERENCES auth_sessions (id) ON DELETE SET NULL;
-- Required for platform force actions, optional but encouraged elsewhere.
ALTER TABLE audit_logs ADD COLUMN reason TEXT;

-- "Show me everything that happened under this impersonation."
CREATE INDEX audit_logs_impersonation_idx
    ON audit_logs (impersonation_session_id)
    WHERE impersonation_session_id IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS audit_logs_impersonation_idx;
ALTER TABLE audit_logs DROP COLUMN IF EXISTS reason;
ALTER TABLE audit_logs DROP COLUMN IF EXISTS impersonation_session_id;
ALTER TABLE audit_logs DROP COLUMN IF EXISTS effective_user_id;
