-- +goose Up
-- Closes the Supabase Security Advisor finding "RLS Disabled in Public", and
-- closes it for good.
--
-- 00004_enable_rls enumerated the tables that existed when it was written. That
-- list drifted twice: `organizations` was missed from the start, and
-- `cleanup_archive` (00006) was created two migrations later and never added.
-- A third hand-maintained list would drift again, so this migration asks the
-- catalog instead of trusting a list: every ordinary table in `public` that
-- does not already have RLS gets it.
--
-- As in 00004, no policies are added. tourney-api connects as `postgres`, which
-- carries BYPASSRLS, so API behavior is unchanged. What this closes is the
-- Supabase anon/authenticated keys, which are public by design: RLS enabled
-- with zero policies denies them by default.
--
-- Adding a table in a later migration? You do not need to touch this file, but
-- do add `ALTER TABLE <name> ENABLE ROW LEVEL SECURITY;` to that migration —
-- this one has already run and will not run again.

-- +goose StatementBegin
DO $$
DECLARE
    t regclass;
BEGIN
    FOR t IN
        SELECT c.oid::regclass
        FROM pg_class c
        JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE n.nspname = 'public'
          AND c.relkind = 'r'              -- ordinary tables only, not views
          AND NOT c.relrowsecurity
          AND c.relname <> 'goose_db_version'  -- goose's own bookkeeping
    LOOP
        EXECUTE format('ALTER TABLE %s ENABLE ROW LEVEL SECURITY', t);
    END LOOP;
END $$;
-- +goose StatementEnd

-- +goose Down
-- Deliberately empty. Down would have to re-disable RLS, which reopens a
-- CRITICAL security finding, and this migration does not record which tables
-- it changed. Roll the schema back past 00004 if you truly need RLS off.
SELECT 1;
