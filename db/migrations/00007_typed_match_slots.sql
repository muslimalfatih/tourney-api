-- +goose Up
-- Typed match-slot sources (Refactor Phase 3.1).
--
-- Until now a slot's origin was implicit: a free-text label in `source` jsonb
-- for group placements ("Winner Group A"), the progression graph for
-- winner-feeds, and nothing at all for byes/empties. Resolution matched label
-- STRINGS against group names, which broke silently in two known ways:
-- ranks past 2 never resolved, and custom group names never matched the
-- letter-based labels. The typed columns make the source machine-readable;
-- the jsonb label remains as display text only.
--
--   fixed            participant assigned at build time
--   group_placement  source_group_id + source_rank (1-based standing)
--   match_winner     source_match_id (the feeder match)
--   match_loser      reserved for a future consolation bracket; never written
--   bye              auto-advance placeholder, distinct from...
--   empty            an unresolved slot that must never silently play
CREATE TYPE slot_source AS ENUM
    ('fixed', 'group_placement', 'match_winner', 'match_loser', 'bye', 'empty');

ALTER TABLE match_participants
    ADD COLUMN source_type     slot_source,
    ADD COLUMN source_group_id UUID REFERENCES groups (id)  ON DELETE SET NULL,
    ADD COLUMN source_rank     INTEGER,
    ADD COLUMN source_match_id UUID REFERENCES matches (id) ON DELETE SET NULL;

-- ---- Backfill, most-specific rule first; each step only touches rows the
-- ---- previous steps left untyped.

-- 1. Group placements: parse the legacy label against the actual group names
--    of the same event's group stage. Covers Winner / Runner-up / "#N".
UPDATE match_participants mp
SET source_type     = 'group_placement',
    source_group_id = g.id,
    source_rank     = CASE
        WHEN mp.source->>'label' = 'Winner '    || g.name THEN 1
        WHEN mp.source->>'label' = 'Runner-up ' || g.name THEN 2
        ELSE (regexp_match(mp.source->>'label', '^#([0-9]+) '))[1]::int
    END
FROM matches m
JOIN stages st  ON st.id = m.stage_id AND st.kind = 'knockout'
JOIN stages gst ON gst.event_id = st.event_id AND gst.kind = 'group'
JOIN groups g   ON g.stage_id = gst.id
WHERE mp.match_id = m.id
  AND mp.source_type IS NULL
  AND mp.source->>'label' IS NOT NULL
  AND (   mp.source->>'label' = 'Winner '    || g.name
       OR mp.source->>'label' = 'Runner-up ' || g.name
       OR mp.source->>'label' ~ ('^#[0-9]+ ' || g.name || '$'));

-- 2. Winner feeds: any slot another match's progression points at.
UPDATE match_participants mp
SET source_type = 'match_winner', source_match_id = feeder.id
FROM matches feeder
WHERE feeder.next_match_id = mp.match_id
  AND feeder.next_slot     = mp.slot
  AND mp.source_type IS NULL;

-- 3. Byes: the empty side of a bye-status match.
UPDATE match_participants mp
SET source_type = 'bye'
FROM matches m
WHERE mp.match_id = m.id AND m.status = 'bye'
  AND mp.participant_id IS NULL AND mp.source_type IS NULL;

-- 4. Fixed entries: a participant assigned and nothing above claimed the slot.
UPDATE match_participants
SET source_type = 'fixed'
WHERE participant_id IS NOT NULL AND source_type IS NULL;

-- 5. Whatever remains is an unresolved empty slot.
UPDATE match_participants SET source_type = 'empty' WHERE source_type IS NULL;

ALTER TABLE match_participants
    ALTER COLUMN source_type SET NOT NULL,
    ADD CONSTRAINT mp_group_placement_ref CHECK
        (source_type <> 'group_placement'
         OR (source_group_id IS NOT NULL AND source_rank >= 1)),
    ADD CONSTRAINT mp_match_ref CHECK
        (source_type NOT IN ('match_winner', 'match_loser')
         OR source_match_id IS NOT NULL);

-- A match must never feed itself (progression-graph sanity; the service also
-- validates cycles at build time).
ALTER TABLE matches
    ADD CONSTRAINT matches_no_self_feed CHECK (next_match_id IS NULL OR next_match_id <> id);

-- +goose Down
ALTER TABLE matches DROP CONSTRAINT IF EXISTS matches_no_self_feed;
ALTER TABLE match_participants
    DROP CONSTRAINT IF EXISTS mp_match_ref,
    DROP CONSTRAINT IF EXISTS mp_group_placement_ref,
    DROP COLUMN IF EXISTS source_match_id,
    DROP COLUMN IF EXISTS source_rank,
    DROP COLUMN IF EXISTS source_group_id,
    DROP COLUMN IF EXISTS source_type;
DROP TYPE IF EXISTS slot_source;
