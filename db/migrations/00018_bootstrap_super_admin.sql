-- +goose Up
-- Subphase 5.5 — bootstrap the first super admin.
--
-- This is a MIGRATION rather than a seed on purpose. Fly's release_command runs
-- `migrate up` on every deploy and never runs `make seed`, so a migration is
-- the only path that reaches production without a human remembering to.
--
-- After this runs, authorization depends on users.role and nothing else. The
-- address below appears in this file and nowhere in runtime code -- there is
-- no email-string comparison anywhere in an authorization path, and promoting
-- or demoting this person later is an ordinary role change.
--
-- Idempotent: re-running converges on the same state. It refuses to run if the
-- address already belongs to an ORGANIZER with tournaments, because silently
-- turning that account into a super admin (org_id must become NULL) would
-- orphan their organization's work -- that is a decision for a human.

-- +goose StatementBegin
DO $$
DECLARE
    v_email   TEXT := lower('kgobang570@gmail.com');
    v_user_id UUID;
    v_role    user_role;
    v_org     UUID;
    v_owned   INT;
BEGIN
    SELECT id, role, org_id INTO v_user_id, v_role, v_org
    FROM users WHERE lower(email) = v_email;

    IF v_user_id IS NOT NULL AND v_role = 'organizer' THEN
        SELECT count(*) INTO v_owned FROM tournaments WHERE org_id = v_org;
        IF v_owned > 0 THEN
            RAISE EXCEPTION
                'bootstrap: % is an organizer whose organization owns % tournament(s); '
                'promote by hand after deciding what happens to that organization',
                v_email, v_owned;
        END IF;
    END IF;

    -- The invitation: an accepted super_admin row with no organization.
    -- Scoped to the active partial index so it can never revive a revoked row.
    INSERT INTO invitations (email, email_display, role, organization_id, status, accepted_at, note)
    VALUES (v_email, 'kgobang570@gmail.com', 'super_admin', NULL, 'accepted', now(), 'bootstrap super admin')
    ON CONFLICT (email) WHERE status <> 'revoked'
    DO UPDATE SET role = 'super_admin', organization_id = NULL,
                  status = 'accepted', accepted_at = coalesce(invitations.accepted_at, now());

    -- The account. org_id must be NULL for a super admin (users_org_scope).
    INSERT INTO users (email, name, role, org_id, status, password_hash)
    VALUES (v_email, 'Super Admin', 'super_admin', NULL, 'active', NULL)
    ON CONFLICT (lower(email))
    DO UPDATE SET role = 'super_admin', org_id = NULL, status = 'active';

    -- Record the promotion so it is visible in the platform audit log rather
    -- than only in git history.
    SELECT id INTO v_user_id FROM users WHERE lower(email) = v_email;
    INSERT INTO audit_logs (actor_user_id, action, target_type, target_id, reason, diff)
    VALUES (NULL, 'admin.bootstrap_super_admin', 'user', v_user_id::text,
            'migration 00018', jsonb_build_object('email', v_email, 'role', 'super_admin'));
END $$;
-- +goose StatementEnd

-- +goose Down
-- Deliberately does not delete the user or invitation: rolling back a schema
-- migration should not remove an account. Only the audit row is withdrawn.
DELETE FROM audit_logs WHERE action = 'admin.bootstrap_super_admin' AND reason = 'migration 00018';
