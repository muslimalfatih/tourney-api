-- +goose Up
-- Public URL identifier for a division, so shared links read
--   /tournaments/bali-open-2026/mixed-doubles/groups
-- instead of carrying a 36-char UUID plus redundant category/gender params.
--
-- Unique per tournament, NOT globally: the tournament slug already scopes the
-- URL, so two tournaments may each have a `mixed-doubles`.
--
-- Slugs are STORED, not derived from events.name at read time. Deriving would
-- silently break every shared link the moment an organizer renames a division,
-- which defeats the point of a URL you can print on a poster.
ALTER TABLE events ADD COLUMN slug TEXT;

-- One-time backfill. Deliberately simpler than the Go slugifier in
-- internal/event/slug.go, which owns every row written from here on: this only
-- has to produce values that are valid, unique and readable for rows that
-- already exist. No unaccent extension — non-[a-z0-9] runs collapse to dashes,
-- and anything that slugs away to nothing falls back to 'division' so the
-- column is never empty.
UPDATE events
SET slug = COALESCE(
    NULLIF(
        regexp_replace(
            regexp_replace(lower(name), '[^a-z0-9]+', '-', 'g'),
            '(^-+|-+$)', '', 'g'
        ),
        ''
    ),
    'division'
);

-- De-duplicate within each tournament. Ordered by the same keys the API lists
-- events by, so the first-listed division keeps the clean slug.
WITH dupes AS (
    SELECT id,
           row_number() OVER (
               PARTITION BY tournament_id, slug
               ORDER BY public_order, created_at, id
           ) AS n
    FROM events
)
UPDATE events e
SET slug = e.slug || '-' || d.n
FROM dupes d
WHERE d.id = e.id AND d.n > 1;

ALTER TABLE events ALTER COLUMN slug SET NOT NULL;
CREATE UNIQUE INDEX events_tournament_slug_key ON events (tournament_id, slug);

-- +goose Down
DROP INDEX IF EXISTS events_tournament_slug_key;
ALTER TABLE events DROP COLUMN IF EXISTS slug;
