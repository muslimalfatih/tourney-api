-- +goose Up
-- Scoring validation foundations (Refactor Phase 3.4).
--
-- Two additive enum values give walkover/retired/cancelled their own states
-- (walkover already existed but was unreachable; retired/cancelled are new).
-- PG 12+ allows ADD VALUE inside a transaction as long as the new value is not
-- used in the same transaction — nothing below uses them.
ALTER TYPE match_status ADD VALUE IF NOT EXISTS 'retired';
ALTER TYPE match_status ADD VALUE IF NOT EXISTS 'cancelled';

-- Per-division scoring configuration, validated in Go on write:
--   best_of:      1 | 3            (default 3)
--   deciding_set: 'full' | 'match_tiebreak'   (default 'full')
--   golden_point: bool             (padel; stored for display, no set-level
--                                   validation impact — per-game data is not
--                                   recorded)
-- '{}' means "all defaults". This replaces the dead scoring_profiles
-- indirection, which stays untouched on the approved candidate-drop list.
ALTER TABLE events ADD COLUMN scoring JSONB NOT NULL DEFAULT '{}'::jsonb;

-- +goose Down
ALTER TABLE events DROP COLUMN IF EXISTS scoring;
-- Enum values are intentionally left in place: PostgreSQL cannot drop enum
-- values, and they are additive/harmless.
