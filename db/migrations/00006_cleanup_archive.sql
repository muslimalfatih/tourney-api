-- +goose Up
-- Generic archive for dev-data cleanup (Refactor Phase 2A). Rows removed by
-- cmd/cleanup are snapshotted here first — full row payload as jsonb, keyed by
-- source table + id — so any cleanup is reversible via `cmd/cleanup -restore`.
--
-- Deliberately generic (table name + jsonb) rather than per-table archive
-- twins: this exists for occasional, explicit, tool-driven cleanup of known
-- rows, not as a soft-delete mechanism the application reads through. Nothing
-- in the API ever queries this table, so archived rows are invisible to every
-- public and organizer surface by construction.
CREATE TABLE cleanup_archive (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    src_table   TEXT        NOT NULL,
    src_id      UUID        NOT NULL,
    payload     JSONB       NOT NULL,
    reason      TEXT        NOT NULL,
    archived_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    restored_at TIMESTAMPTZ,
    UNIQUE (src_table, src_id)
);

-- +goose Down
-- Down drops the archive table. If archived rows should survive, run
-- `go run ./cmd/cleanup -restore` BEFORE migrating down — dropping the table
-- discards any snapshots still in it.
DROP TABLE IF EXISTS cleanup_archive;
