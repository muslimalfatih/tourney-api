//go:build integration

package inttest

import (
	"context"
	"testing"
)

// TestRLSEnabledOnEveryPublicTable is the check that keeps 00004's drift from
// happening a third time. Two tables (organizations, cleanup_archive) sat
// unprotected because the RLS list was maintained by hand; this asserts the
// property directly against the catalog, so a new table without RLS fails here
// rather than surfacing weeks later as a CRITICAL in the Supabase advisor.
func TestRLSEnabledOnEveryPublicTable(t *testing.T) {
	e := setup(t)

	rows, err := e.pool.Query(context.Background(), `
		SELECT c.relname
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public'
		  AND c.relkind = 'r'
		  AND NOT c.relrowsecurity
		  AND c.relname <> 'goose_db_version'
		ORDER BY c.relname`)
	if err != nil {
		t.Fatalf("query pg_class: %v", err)
	}
	defer rows.Close()

	var unprotected []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		unprotected = append(unprotected, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}

	if len(unprotected) > 0 {
		t.Errorf("public tables without row-level security: %v\n"+
			"add `ALTER TABLE <name> ENABLE ROW LEVEL SECURITY;` to the migration that creates each one",
			unprotected)
	}
}
