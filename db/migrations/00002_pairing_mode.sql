-- +goose Up
-- Match builder: how an event's round-1 pairs are decided. 'auto' = random pairs
-- from the participant list (the existing seeded generator with seeds ignored);
-- 'manual' = the organizer supplied explicit round-1 pairings. Default 'auto'
-- keeps every existing event's behaviour unchanged.
CREATE TYPE pairing_mode AS ENUM ('auto', 'manual');

ALTER TABLE events
    ADD COLUMN pairing_mode pairing_mode NOT NULL DEFAULT 'auto';

-- +goose Down
ALTER TABLE events DROP COLUMN pairing_mode;
DROP TYPE pairing_mode;
