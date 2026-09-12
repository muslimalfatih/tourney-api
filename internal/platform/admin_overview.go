package platform

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/muslimalfatih/tourney-api/internal/audit"
	"github.com/muslimalfatih/tourney-api/internal/auth"
)

const ActionPlunkTestSent = "admin.plunk_test_otp_sent"

// Overview is the platform control-center summary: operational counts, not
// analytics. Every number here answers "is something wrong right now".
type Overview struct {
	Organizers          int `json:"organizers"`
	ActiveUsers         int `json:"active_users"`
	SuspendedUsers      int `json:"suspended_users"`
	SuperAdmins         int `json:"super_admins"`
	PendingInvitations  int `json:"pending_invitations"`
	RevokedInvitations  int `json:"revoked_invitations"`
	Tournaments         int `json:"tournaments"`
	DraftTournaments    int `json:"draft_tournaments"`
	PublishedTournament int `json:"published_tournaments"`
	ArchivedTournaments int `json:"archived_tournaments"`
	LiveMatches         int `json:"live_matches"`
	ActiveImpersonation int `json:"active_impersonations"`

	RecentAuth  []audit.Log `json:"recent_auth"`
	RecentAudit []audit.Log `json:"recent_audit"`
}

func (s *Service) Overview(ctx context.Context) (*Overview, error) {
	var o Overview
	err := s.pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM users WHERE role = 'organizer'),
		  (SELECT count(*) FROM users WHERE status = 'active'),
		  (SELECT count(*) FROM users WHERE status = 'suspended'),
		  (SELECT count(*) FROM users WHERE role = 'super_admin' AND status = 'active'),
		  (SELECT count(*) FROM invitations WHERE status = 'pending' AND expires_at > now()),
		  (SELECT count(*) FROM invitations WHERE status = 'revoked'),
		  (SELECT count(*) FROM tournaments),
		  (SELECT count(*) FROM tournaments WHERE status = 'draft'),
		  (SELECT count(*) FROM tournaments WHERE status = 'published'),
		  (SELECT count(*) FROM tournaments WHERE status = 'archived'),
		  (SELECT count(*) FROM matches WHERE status = 'live'),
		  (SELECT count(*) FROM auth_sessions
		     WHERE kind = 'impersonation' AND revoked_at IS NULL AND expires_at > now())`).
		Scan(&o.Organizers, &o.ActiveUsers, &o.SuspendedUsers, &o.SuperAdmins,
			&o.PendingInvitations, &o.RevokedInvitations,
			&o.Tournaments, &o.DraftTournaments, &o.PublishedTournament, &o.ArchivedTournaments,
			&o.LiveMatches, &o.ActiveImpersonation)
	if err != nil {
		return nil, err
	}

	// Two short feeds. Auth events are those whose action starts with "auth.";
	// the general feed is everything else.
	all, _, err := s.audit.List(ctx, audit.ListFilter{}, 40, 0)
	if err != nil {
		return nil, err
	}
	o.RecentAuth = []audit.Log{}
	o.RecentAudit = []audit.Log{}
	for _, l := range all {
		if len(l.Action) > 5 && l.Action[:5] == "auth." {
			if len(o.RecentAuth) < 10 {
				o.RecentAuth = append(o.RecentAuth, l)
			}
		} else if len(o.RecentAudit) < 10 {
			o.RecentAudit = append(o.RecentAudit, l)
		}
	}
	return &o, nil
}

// Settings is the read-only v1 platform settings view. Nothing here is
// writable yet, and no secret value ever appears — only whether one is set.
type Settings struct {
	DefaultTimezone     string `json:"default_timezone"`
	DefaultTimezoneNote string `json:"default_timezone_note"`
	PlunkConfigured     bool   `json:"plunk_configured"`
	PlunkFromEmail      string `json:"plunk_from_email,omitempty"`
	PlunkFromName       string `json:"plunk_from_name,omitempty"`
	PasswordLoginOn     bool   `json:"password_login_enabled"`
	RealSendAllowed     bool   `json:"real_send_allowed"`
	Environment         string `json:"environment"`
}

// SettingsSource is the subset of config the settings page reads. A struct
// rather than the whole config, so the handler cannot reach a secret by
// accident.
type SettingsSource struct {
	DefaultTimezone string
	PlunkConfigured bool
	PlunkFromEmail  string
	PlunkFromName   string
	PasswordLoginOn bool
	RealSendAllowed bool
	Environment     string
}

func (s *Service) Settings(src SettingsSource) *Settings {
	return &Settings{
		DefaultTimezone:     src.DefaultTimezone,
		DefaultTimezoneNote: "Applies only to new tournaments. Existing tournaments keep their own timezone.",
		PlunkConfigured:     src.PlunkConfigured,
		PlunkFromEmail:      src.PlunkFromEmail,
		PlunkFromName:       src.PlunkFromName,
		PasswordLoginOn:     src.PasswordLoginOn,
		RealSendAllowed:     src.RealSendAllowed,
		Environment:         src.Environment,
	}
}

// SendTestOTP sends a real sign-in code to the CALLER's own address, through
// the exact path a normal login uses, so what arrives is what an organizer
// would see.
//
// The recipient is taken from the caller's account row, never from the
// request. The normal per-email rate limit applies, so this cannot be used to
// spam even the admin's own inbox.
func (s *Service) SendTestOTP(ctx context.Context, by Attribution, otp *auth.OTPService, ip, ua string) error {
	var email string
	if err := s.pool.QueryRow(ctx, `SELECT email FROM users WHERE id = $1`, by.Actor).Scan(&email); err != nil {
		return err
	}
	if err := otp.Request(ctx, auth.RequestParams{Email: email, IP: ip, UserAgent: ua}); err != nil {
		return err
	}
	_ = s.audit.Record(ctx, by.entry(audit.Entry{
		Action: ActionPlunkTestSent, TargetType: "user", TargetID: by.Actor.String(),
		Diff: map[string]any{"email": email, "at": time.Now().UTC()},
	}))
	return nil
}

var _ = uuid.Nil // keep import stable if unused in some builds
