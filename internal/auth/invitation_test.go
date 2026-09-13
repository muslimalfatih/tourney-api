package auth

import (
	"testing"
	"time"
)

// The active rule decides who can sign in at all, so it is worth pinning down
// precisely rather than inferring from the SQL.
func TestInvitation_IsActive(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)

	cases := []struct {
		name    string
		inv     Invitation
		want    bool
		because string
	}{
		{"pending, not expired", Invitation{Status: InvitePending, ExpiresAt: future}, true,
			"the normal case"},
		{"pending, expired", Invitation{Status: InvitePending, ExpiresAt: past}, false,
			"a 14-day-old unused invitation should not still admit someone"},
		{"accepted, expiry in the past", Invitation{Status: InviteAccepted, ExpiresAt: past}, true,
			"an expiry date must never lock someone out of an account they already use"},
		{"accepted, expiry in the future", Invitation{Status: InviteAccepted, ExpiresAt: future}, true,
			"accepted ignores expiry either way"},
		{"revoked, not expired", Invitation{Status: InviteRevoked, ExpiresAt: future}, false,
			"revocation beats everything"},
		{"revoked and accepted-then-revoked", Invitation{Status: InviteRevoked, ExpiresAt: past}, false,
			"revoked is terminal"},
	}
	for _, c := range cases {
		if got := c.inv.IsActive(); got != c.want {
			t.Errorf("%s: IsActive() = %v, want %v (%s)", c.name, got, c.want, c.because)
		}
	}
}

func TestUser_IsActive(t *testing.T) {
	if !(&User{Status: StatusActive}).IsActive() {
		t.Error("active user reported inactive")
	}
	if (&User{Status: StatusSuspended}).IsActive() {
		t.Error("suspended user reported active")
	}
	// An unset status must not read as active: a zero-value User appearing
	// anywhere is a bug, and failing closed is the safe direction.
	if (&User{}).IsActive() {
		t.Error("zero-value user must not be treated as active")
	}
}
