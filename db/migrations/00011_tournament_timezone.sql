-- +goose Up
-- Phase 3.6: tournament-local presentation timezone. Validated IANA text (via
-- Go time.LoadLocation at the write path), NOT an enum — the IANA set evolves
-- and Postgres can't validate names portably. Storage everywhere stays UTC
-- TIMESTAMPTZ; this column only drives tournament-local date grouping and
-- display. Conflict math (Phase 3.5) is instant-based and unaffected.
ALTER TABLE tournaments
    ADD COLUMN timezone TEXT NOT NULL DEFAULT 'Asia/Makassar';

-- +goose Down
ALTER TABLE tournaments DROP COLUMN IF EXISTS timezone;
