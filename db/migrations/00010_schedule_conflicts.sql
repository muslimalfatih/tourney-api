-- +goose Up
-- Phase 3.5: scheduling integrity. schedule_slots is the authoritative
-- schedule model (matches.scheduled_at/court_id are derived stamps mirrored on
-- slot writes), and it already carries explicit starts_at/ends_at — so no
-- duration column is needed; the exclusion constraint ranges over the stored
-- interval directly. Half-open '[)' ranges make back-to-back slots legal.
CREATE EXTENSION IF NOT EXISTS btree_gist;

-- Pre-constraint data fix: three overlapping demo slots existed on Bali Open
-- 2026's Centre Court (two exact duplicates + one straddling both). Archive
-- the losers into cleanup_archive (the Phase 2 snapshot table), then remove
-- them. Restore (after dropping the constraint) is one line of SQL per row:
--   INSERT INTO schedule_slots
--   SELECT * FROM cleanup_archive, jsonb_populate_record(NULL::schedule_slots, payload)
--   WHERE src_table = 'schedule_slots';  -- (cmd/cleanup -restore only covers matches)
-- Rule: a slot loses if it overlaps a slot ordered earlier by (starts_at, id).
-- This can over-remove in pathological chains, but never under-removes, and
-- every removed row is archived first.
-- +goose StatementBegin
INSERT INTO cleanup_archive (src_table, src_id, payload, reason)
SELECT 'schedule_slots', b.id, to_jsonb(b),
       'migration 00010: overlapping slot removed before court-overlap exclusion constraint'
FROM schedule_slots b
WHERE EXISTS (
    SELECT 1 FROM schedule_slots a
    WHERE a.court_id = b.court_id
      AND (a.starts_at, a.id) < (b.starts_at, b.id)
      AND tstzrange(a.starts_at, a.ends_at, '[)') && tstzrange(b.starts_at, b.ends_at, '[)'))
ON CONFLICT (src_table, src_id) DO NOTHING;
-- +goose StatementEnd

-- +goose StatementBegin
DELETE FROM schedule_slots b
WHERE EXISTS (
    SELECT 1 FROM schedule_slots a
    WHERE a.court_id = b.court_id
      AND (a.starts_at, a.id) < (b.starts_at, b.id)
      AND tstzrange(a.starts_at, a.ends_at, '[)') && tstzrange(b.starts_at, b.ends_at, '[)'));
-- +goose StatementEnd

-- Empty/inverted ranges would slip through the exclusion constraint (an empty
-- tstzrange overlaps nothing), so a CHECK closes that gap.
ALTER TABLE schedule_slots
    ADD CONSTRAINT schedule_slots_time_valid CHECK (ends_at > starts_at);

-- The last-resort guarantee: no two slots on one court may overlap in time.
-- Courts are tournament-scoped (courts -> venues -> tournament), so court_id
-- equality already implies same tournament. The service validates first and
-- returns structured 422s; this constraint only fires on true write races,
-- surfacing as SQLSTATE 23P01 -> HTTP 409.
ALTER TABLE schedule_slots
    ADD CONSTRAINT schedule_slots_no_court_overlap
    EXCLUDE USING gist (court_id WITH =, tstzrange(starts_at, ends_at, '[)') WITH &&);

-- +goose Down
ALTER TABLE schedule_slots DROP CONSTRAINT IF EXISTS schedule_slots_no_court_overlap;
ALTER TABLE schedule_slots DROP CONSTRAINT IF EXISTS schedule_slots_time_valid;
-- Extension left installed (harmless; other objects may come to depend on it).
-- Archived slots stay restorable via the SQL documented in the Up section.
