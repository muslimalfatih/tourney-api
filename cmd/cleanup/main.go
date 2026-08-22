// Command cleanup reversibly removes a fixed, fingerprinted list of known
// dev-data debris (Refactor Phase 2A). It is deliberately NOT a general
// deletion tool: the target rows are hardcoded below, and every row is
// re-verified against its recorded fingerprint before anything is touched, so
// running it against a database where the rows have changed does nothing.
//
//	go run ./cmd/cleanup            # report only — no writes
//	go run ./cmd/cleanup -archive   # snapshot to cleanup_archive, then delete
//	go run ./cmd/cleanup -restore   # re-insert archived rows from snapshots
//
// All modes are idempotent: -archive skips rows already archived or no longer
// matching their fingerprint; -restore skips rows already restored or never
// archived. Both run in a single transaction.
//
// Origin of the target list: 14 ad-hoc round-robin fixtures in Renon Cup
// 2026's "Women's Doubles Beginner++" division (match_no 1-14), created by
// repeated manual submits of the organizer RoundRobinBuilder during
// interactive testing on 2026-08-13 — the pairings contain duplicates, which
// no generator produces (GenerateRoundRobin emits each pairing exactly once),
// and none carry a stage_id (the manual-fixture path writes none). Rows 1-11
// are still 'pending'; 12-14 are stuck 'scheduled' after their (wrong-dated)
// schedule slots were removed, which makes them un-deletable through
// DeleteManualMatch (it refuses non-pending rows). None have score rows. The
// deterministic Renon Cup seed (cmd/seed) rebuilds this division's fixture
// list cleanly by natural key once these are archived.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/muslimalfatih/laga-api/internal/config"
	"github.com/muslimalfatih/laga-api/internal/storage/postgres"
)

// womensEventID is the "Women's Doubles Beginner++" division of renon-cup-2026.
const womensEventID = "c052ae22-96ea-4326-9e75-072340e25e91"

// target is one row scheduled for archival, with the fingerprint it must still
// match. A mismatch (different event, different match_no, has scores, has
// slots) means the database has drifted from what this tool was written
// against — the row is skipped and reported, never guessed at.
type target struct {
	matchID string
	matchNo int
}

var targets = []target{
	{"3abab90f-0d37-40cb-abf4-6e851157e663", 1},
	{"cc43227a-9ab8-41e0-a8d3-3397479c3cfb", 2},
	{"b4d1a57c-dede-4df1-82c1-b4e17273742e", 3},
	{"1fc7b864-9041-49a7-a751-94267f6ff800", 4},
	{"d8c05eab-eb96-4169-84e1-0596a04d060c", 5},
	{"e4e7b1f9-f42b-4b7e-9e23-c00b688ae8e0", 6},
	{"04579fea-2d93-470b-903b-845020bba144", 7},
	{"05fce083-7ed0-4acf-8c07-2d482d4d529d", 8},
	{"2655ef8d-1060-4266-837f-ce2512e4ed2e", 9},
	{"1c7d7f39-32bd-4e73-8eaf-c36996ba7d5f", 10},
	{"4db16433-94e3-46bc-96e5-6e3a20b2997d", 11},
	{"3beec65d-72e8-4815-a2b4-663c60f1be45", 12},
	{"62877e8f-bc95-4ceb-b796-436256d8c9ff", 13},
	{"7d1a58dc-ac1e-46b6-b346-d590f0ae9cb8", 14},
}

func main() {
	archive := flag.Bool("archive", false, "snapshot the target rows into cleanup_archive, then delete them")
	restore := flag.Bool("restore", false, "re-insert previously archived rows from their snapshots")
	flag.Parse()

	if err := run(*archive, *restore); err != nil {
		slog.Error("cleanup failed", slog.Any("error", err))
		os.Exit(1)
	}
}

func run(archive, restore bool) error {
	if archive && restore {
		return fmt.Errorf("-archive and -restore are mutually exclusive")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := postgres.Connect(ctx, cfg.DatabaseURL, 2)
	if err != nil {
		return err
	}
	defer pool.Close()

	switch {
	case archive:
		return archiveTargets(ctx, pool)
	case restore:
		return restoreTargets(ctx, pool)
	default:
		return report(ctx, pool)
	}
}

// verifyFingerprint returns "" when the live row still matches what this tool
// was written against, or a human-readable reason to skip it.
func verifyFingerprint(ctx context.Context, q pgx.Tx, t target) (string, error) {
	var (
		eventID   string
		matchNo   int
		status    string
		scoreRows int
		slotRows  int
	)
	err := q.QueryRow(ctx, `
		SELECT m.event_id::text, m.match_no, m.status::text,
		       (SELECT count(*) FROM match_scores ms WHERE ms.match_id = m.id),
		       (SELECT count(*) FROM schedule_slots ss WHERE ss.match_id = m.id)
		FROM matches m WHERE m.id = $1`, t.matchID).
		Scan(&eventID, &matchNo, &status, &scoreRows, &slotRows)
	if err == pgx.ErrNoRows {
		return "row no longer exists", nil
	}
	if err != nil {
		return "", err
	}
	switch {
	case eventID != womensEventID:
		return "belongs to a different event", nil
	case matchNo != t.matchNo:
		return fmt.Sprintf("match_no drifted (now %d)", matchNo), nil
	case status != "pending" && status != "scheduled":
		return fmt.Sprintf("status %q — may have been played", status), nil
	case scoreRows != 0:
		return "has score rows", nil
	case slotRows != 0:
		return "has schedule slots", nil
	}
	return "", nil
}

func report(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) // read-only; always rolled back

	fmt.Println("== Phase 2A cleanup report (no writes) ==")
	for _, t := range targets {
		reason, err := verifyFingerprint(ctx, tx, t)
		if err != nil {
			return err
		}
		var archived bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM cleanup_archive WHERE src_table='matches' AND src_id=$1 AND restored_at IS NULL)`,
			t.matchID).Scan(&archived); err != nil {
			return err
		}
		state := "ELIGIBLE"
		if archived {
			state = "already archived"
		} else if reason != "" {
			state = "skip: " + reason
		}
		fmt.Printf("  match_no %-3d %s  %s\n", t.matchNo, t.matchID, state)
	}
	return nil
}

func archiveTargets(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	archived, skipped := 0, 0
	for _, t := range targets {
		var already bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM cleanup_archive WHERE src_table='matches' AND src_id=$1 AND restored_at IS NULL)`,
			t.matchID).Scan(&already); err != nil {
			return err
		}
		if already {
			skipped++
			continue
		}
		reason, err := verifyFingerprint(ctx, tx, t)
		if err != nil {
			return err
		}
		if reason != "" {
			fmt.Printf("  skip match_no %d (%s): %s\n", t.matchNo, t.matchID, reason)
			skipped++
			continue
		}

		// Snapshot the match row and its participant rows into one payload,
		// then delete. jsonb passed as text with an explicit cast — required
		// under the pooler's simple protocol (see audit.Record).
		if _, err := tx.Exec(ctx, `
			INSERT INTO cleanup_archive (src_table, src_id, reason, payload)
			SELECT 'matches', m.id, 'phase-2a ad-hoc RR fixture (manual duplicate)',
			       jsonb_build_object(
			           'match', to_jsonb(m),
			           'participants', COALESCE((
			               SELECT jsonb_agg(to_jsonb(mp))
			               FROM match_participants mp WHERE mp.match_id = m.id
			           ), '[]'::jsonb)
			       )
			FROM matches m WHERE m.id = $1
			ON CONFLICT (src_table, src_id) DO UPDATE SET restored_at = NULL, archived_at = now()`,
			t.matchID); err != nil {
			return fmt.Errorf("snapshot match %s: %w", t.matchID, err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM match_participants WHERE match_id = $1`, t.matchID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM matches WHERE id = $1`, t.matchID); err != nil {
			return err
		}
		archived++
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	fmt.Printf("archived %d, skipped %d (of %d targets)\n", archived, skipped, len(targets))
	return nil
}

func restoreTargets(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	restored, skipped := 0, 0
	for _, t := range targets {
		var payload []byte
		err := tx.QueryRow(ctx, `
			SELECT payload FROM cleanup_archive
			WHERE src_table = 'matches' AND src_id = $1 AND restored_at IS NULL`,
			t.matchID).Scan(&payload)
		if err == pgx.ErrNoRows {
			skipped++
			continue
		}
		if err != nil {
			return err
		}

		// Rebuild the match row, then its participant rows, from the snapshot.
		if _, err := tx.Exec(ctx, `
			INSERT INTO matches
			SELECT * FROM jsonb_populate_record(NULL::matches, ($1::jsonb)->'match')
			ON CONFLICT (id) DO NOTHING`, string(payload)); err != nil {
			return fmt.Errorf("restore match %s: %w", t.matchID, err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO match_participants
			SELECT * FROM jsonb_populate_recordset(NULL::match_participants, ($1::jsonb)->'participants')
			ON CONFLICT (id) DO NOTHING`, string(payload)); err != nil {
			return fmt.Errorf("restore participants %s: %w", t.matchID, err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE cleanup_archive SET restored_at = now()
			WHERE src_table = 'matches' AND src_id = $1`, t.matchID); err != nil {
			return err
		}
		restored++
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	fmt.Printf("restored %d, skipped %d (of %d targets)\n", restored, skipped, len(targets))
	return nil
}
