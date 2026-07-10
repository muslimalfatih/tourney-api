-- +goose Up
-- Public-facing event configuration + a tournament description + per-group
-- advance count. These drive the public category strip, gender/phase filters,
-- and per-event visibility. `has_group_stage`/`has_knockout_stage` are
-- deliberately NOT stored — they are derived from the existing stages table and
-- events.format ('group_knockout' → both; 'round_robin' → group only;
-- 'single_elim' → knockout only), so there is a single source of truth.

CREATE TYPE event_gender AS ENUM ('men', 'women', 'mixed');

ALTER TABLE events
    ADD COLUMN category             TEXT,                                    -- organizer-defined (nullable = uncategorised)
    ADD COLUMN gender               event_gender NOT NULL DEFAULT 'mixed',
    ADD COLUMN is_public            BOOLEAN      NOT NULL DEFAULT true,
    ADD COLUMN public_display_name  TEXT,                                    -- NULL → fall back to events.name
    ADD COLUMN public_order         INTEGER      NOT NULL DEFAULT 0;

-- How many top teams advance out of a group (shown on the public footnote and
-- used by group→knockout resolution). Default 2 matches the common bracket.
ALTER TABLE groups
    ADD COLUMN advance_count INTEGER NOT NULL DEFAULT 2;

ALTER TABLE tournaments
    ADD COLUMN description TEXT;

-- Category strip ordering per tournament.
CREATE INDEX idx_events_public_order ON events (tournament_id, public_order);

-- +goose Down
DROP INDEX IF EXISTS idx_events_public_order;
ALTER TABLE tournaments DROP COLUMN description;
ALTER TABLE groups DROP COLUMN advance_count;
ALTER TABLE events
    DROP COLUMN public_order,
    DROP COLUMN public_display_name,
    DROP COLUMN is_public,
    DROP COLUMN gender,
    DROP COLUMN category;
DROP TYPE event_gender;
