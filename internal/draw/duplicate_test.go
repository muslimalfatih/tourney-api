package draw

import (
	"testing"

	"github.com/google/uuid"
)

// TestClassifyDuplicate covers the Phase 3.7 decision table: which existing
// fixtures block a create, which permit an audited rematch, and how
// allow_rematch interacts with each.
func TestClassifyDuplicate(t *testing.T) {
	mk := func(no int, status string) dupRow {
		return dupRow{id: uuid.New(), matchNo: no, status: status}
	}
	cases := []struct {
		name         string
		rows         []dupRow
		allowRematch bool
		wantErr      bool
		wantRematch  bool // rematchable flag on the error
		wantAudit    bool // create proceeds as an audited rematch
	}{
		{"no existing fixture", nil, false, false, false, false},
		{"pending blocks", []dupRow{mk(1, "pending")}, false, true, false, false},
		{"scheduled blocks", []dupRow{mk(1, "scheduled")}, false, true, false, false},
		{"live blocks", []dupRow{mk(1, "live")}, false, true, false, false},
		{"pending blocks even with override", []dupRow{mk(1, "pending")}, true, true, false, false},
		{"completed asks for confirmation", []dupRow{mk(1, "completed")}, false, true, true, false},
		{"walkover asks for confirmation", []dupRow{mk(1, "walkover")}, false, true, true, false},
		{"retired asks for confirmation", []dupRow{mk(1, "retired")}, false, true, true, false},
		{"completed + override creates audited rematch", []dupRow{mk(1, "completed")}, true, false, false, true},
		{"decided rematch + still-unplayed second blocks", []dupRow{mk(2, "pending"), mk(1, "completed")}, true, true, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err, audit := classifyDuplicate(tc.rows, tc.allowRematch)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && err.Rematchable != tc.wantRematch {
				t.Errorf("rematchable = %v, want %v", err.Rematchable, tc.wantRematch)
			}
			if audit != tc.wantAudit {
				t.Errorf("audited rematch = %v, want %v", audit, tc.wantAudit)
			}
		})
	}
}
