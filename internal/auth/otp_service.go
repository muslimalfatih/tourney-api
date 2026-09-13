package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/muslimalfatih/tourney-api/internal/audit"
	"github.com/muslimalfatih/tourney-api/internal/email"
)

// Audit actions for the authentication flow.
const (
	ActionOTPRequested       = "auth.otp_requested"
	ActionOTPRequestRejected = "auth.otp_request_rejected"
	ActionOTPRateLimited     = "auth.otp_rate_limited"
	ActionOTPVerified        = "auth.otp_verified"
	ActionOTPFailed          = "auth.otp_failed"
	ActionOTPExpired         = "auth.otp_expired"
	ActionInvitationAccepted = "invitation.accepted"
	ActionLogout             = "auth.logout"
)

// ErrRateLimited is returned when an address or client has asked too often.
var ErrRateLimited = errors.New("too many requests")

// OTPService is the sign-in flow: allowlist, rate limits, code delivery, and
// the transaction that turns a correct code into a session.
type OTPService struct {
	users       *Repository
	invitations *InvitationRepository
	otp         *OTPRepository
	sessions    *SessionRepository
	tokens      *TokenService
	limiter     *Limiter
	sender      email.Sender
	audit       *audit.Service
	log         *slog.Logger
}

func NewOTPService(
	users *Repository,
	invitations *InvitationRepository,
	otp *OTPRepository,
	sessions *SessionRepository,
	tokens *TokenService,
	limiter *Limiter,
	sender email.Sender,
	auditSvc *audit.Service,
	log *slog.Logger,
) *OTPService {
	return &OTPService{
		users: users, invitations: invitations, otp: otp, sessions: sessions,
		tokens: tokens, limiter: limiter, sender: sender, audit: auditSvc, log: log,
	}
}

// hashForAudit reduces an IP or user agent to a correlatable token. These exist
// to spot abuse patterns, not to identify people — an audit trail should not
// double as a location history.
func hashForAudit(v string) *string {
	if v == "" {
		return nil
	}
	sum := sha256.Sum256([]byte(v))
	s := hex.EncodeToString(sum[:])[:32]
	return &s
}

// RequestParams carries the client context for one OTP request.
type RequestParams struct {
	Email     string
	IP        string
	UserAgent string
}

// Request issues and delivers a sign-in code.
//
// Order matters. Rate limits run BEFORE the allowlist lookup, so the endpoint
// cannot be used to enumerate which addresses are invited by timing or by
// counting how many probes it takes to get throttled.
func (s *OTPService) Request(ctx context.Context, p RequestParams) error {
	addr := NormalizeEmail(p.Email)
	if !looksLikeEmail(addr) {
		return ErrInvalidEmail
	}

	if !s.limiter.Allow("otp:email:"+addr, OTPPerEmailLimit, OTPPerEmailWindow) {
		s.record(ctx, ActionOTPRateLimited, addr, nil)
		return ErrRateLimited
	}
	if p.IP != "" && !s.limiter.Allow("otp:ip:"+p.IP, OTPPerIPLimit, OTPPerIPWindow) {
		s.record(ctx, ActionOTPRateLimited, addr, nil)
		return ErrRateLimited
	}

	inv, err := s.invitations.FindActiveByEmail(ctx, addr)
	if err != nil {
		// Both ErrNotInvited and ErrInvitationExpired reach the caller as
		// distinct 403s. That is a deliberate product choice: an invite-only
		// tool that answered vaguely would leave invited people unable to tell
		// a typo from an outage. Nothing beyond "you are not on the list" is
		// disclosed — no hint that a revoked invitation once existed.
		s.record(ctx, ActionOTPRequestRejected, addr, nil)
		return err
	}

	// An account that exists but is suspended must not receive a code at all.
	if user, uerr := s.users.FindByEmail(ctx, addr); uerr == nil && !user.IsActive() {
		s.record(ctx, ActionOTPRequestRejected, addr, nil)
		return ErrAccountSuspended
	}

	code, ch, err := s.otp.Issue(ctx, IssueParams{
		Email:        addr,
		InvitationID: &inv.ID,
		IPHash:       hashForAudit(p.IP),
		UAHash:       hashForAudit(p.UserAgent),
	})
	if err != nil {
		return err
	}

	if err := s.sender.SendOTP(ctx, addr, code); err != nil {
		// The provider's own message never leaves this line: it can carry the
		// recipient list, and on some providers the API key itself.
		//
		// The HTTP status is different — it leaks nothing and is the single
		// most useful thing here. Dropping it too (as this once did) meant a
		// real outage logged only "otp delivery failed", giving no way to tell
		// a rejected key from a wrong endpoint from a provider outage without
		// reproducing the call by hand against production.
		attrs := []any{slog.String("challenge_id", ch.ID.String())}
		var de *email.DeliveryError
		if errors.As(err, &de) {
			attrs = append(attrs, slog.Int("provider_status", de.Status))
		}
		s.log.ErrorContext(ctx, "otp delivery failed", attrs...)
		return ErrDeliveryFailed
	}

	s.record(ctx, ActionOTPRequested, addr, &ch.ID)
	return nil
}

// VerifyAndSignIn checks a code and, on success, opens a session.
//
// Everything after the code verifies happens in ONE transaction: consuming the
// challenge, accepting the invitation, creating the user and opening the
// session either all commit or none do. A partial success here would be the
// worst kind: a burned code and no way in.
func (s *OTPService) VerifyAndSignIn(ctx context.Context, emailAddr, code, ip, ua string) (*TokenPair, *User, error) {
	addr := NormalizeEmail(emailAddr)
	if !looksLikeEmail(addr) {
		return nil, nil, ErrInvalidEmail
	}

	// Re-checked here, not merely at request time: an invitation may have been
	// revoked in the ten minutes since the code was sent.
	inv, err := s.invitations.FindActiveByEmail(ctx, addr)
	if err != nil {
		return nil, nil, err
	}

	tx, err := s.users.pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	ch, hash, err := s.otp.LockLive(ctx, tx, addr, OTPPurposeLogin)
	if err != nil {
		return nil, nil, err
	}
	if err := CheckLive(ch); err != nil {
		if errors.Is(err, ErrOTPExpired) {
			s.record(ctx, ActionOTPExpired, addr, &ch.ID)
		}
		return nil, nil, err
	}

	if !VerifyCode(addr, code, hash, s.otp.pepper) {
		// Release the lock before recording the attempt, which commits on its
		// own so the cap cannot be escaped by dropping the connection.
		_ = tx.Rollback(ctx)
		locked, e := s.otp.RecordFailure(ctx, ch.ID)
		if e != nil {
			return nil, nil, e
		}
		s.record(ctx, ActionOTPFailed, addr, &ch.ID)
		if locked {
			return nil, nil, ErrOTPLocked
		}
		return nil, nil, ErrOTPInvalid
	}

	if err := s.otp.Consume(ctx, tx, ch.ID); err != nil {
		return nil, nil, err
	}

	user, err := s.upsertFromInvitation(ctx, tx, inv)
	if err != nil {
		return nil, nil, err
	}
	if !user.IsActive() {
		return nil, nil, ErrAccountSuspended
	}

	if inv.Status == InvitePending {
		if err := s.invitations.MarkAccepted(ctx, tx, inv.ID); err != nil {
			return nil, nil, err
		}
		_ = s.audit.RecordTx(ctx, tx, audit.Entry{
			ActorUserID: user.ID, Action: ActionInvitationAccepted,
			TargetType: "invitation", TargetID: inv.ID.String(),
		})
	}

	rawRefresh, refreshHash, err := NewRefreshToken()
	if err != nil {
		return nil, nil, err
	}
	sess, err := s.sessions.Create(ctx, tx, CreateParams{
		UserID:        user.ID,
		Kind:          SessionNormal,
		RefreshHash:   refreshHash,
		ExpiresAt:     time.Now().Add(s.tokens.RefreshTTL()),
		IPHash:        hashForAudit(ip),
		UserAgentHash: hashForAudit(ua),
	})
	if err != nil {
		return nil, nil, err
	}

	if _, err := tx.Exec(ctx,
		`UPDATE users SET last_login_at = now() WHERE id = $1`, user.ID); err != nil {
		return nil, nil, err
	}

	_ = s.audit.RecordTx(ctx, tx, audit.Entry{
		ActorUserID: user.ID, Action: ActionOTPVerified,
		TargetType: "user", TargetID: user.ID.String(),
	})

	if err := tx.Commit(ctx); err != nil {
		return nil, nil, err
	}

	// The completed flow should not keep counting against the person who just
	// finished it successfully.
	s.limiter.Reset("otp:email:" + addr)

	access, err := s.tokens.IssueAccess(user.ID, user.Role, user.OrgID, sess.ID)
	if err != nil {
		return nil, nil, err
	}
	return &TokenPair{AccessToken: access, RefreshToken: rawRefresh}, user, nil
}

// upsertFromInvitation returns the account for an invitation, creating it on
// first sign-in.
//
// The invitation sets role and organization ONLY at creation. A returning user
// keeps whatever an admin has since assigned them — otherwise every sign-in
// would silently undo a role change, and the admin screen would appear to lie.
func (s *OTPService) upsertFromInvitation(ctx context.Context, tx pgx.Tx, inv *Invitation) (*User, error) {
	name := inv.Email
	if at := strings.Index(name, "@"); at > 0 {
		name = name[:at]
	}
	if inv.EmailDisplay != nil && *inv.EmailDisplay != "" {
		name = strings.TrimSpace(*inv.EmailDisplay)
		if at := strings.Index(name, "@"); at > 0 {
			name = name[:at]
		}
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO users (email, name, role, org_id, status, password_hash)
		VALUES ($1, $2, $3::user_role, $4, 'active', NULL)
		ON CONFLICT (lower(email)) DO NOTHING`,
		inv.Email, name, inv.Role, inv.OrganizationID); err != nil {
		return nil, err
	}

	var u User
	err := tx.QueryRow(ctx, `
		SELECT id, email, password_hash, name, role, org_id, status
		FROM users WHERE lower(email) = $1`, inv.Email).
		Scan(&u.ID, &u.Email, &u.PasswordHash, &u.Name, &u.Role, &u.OrgID, &u.Status)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// record writes a best-effort audit row. Auditing must never fail the flow it
// is observing, but a failure to audit is itself worth logging.
//
// The code is deliberately absent from every field: a plaintext OTP in an audit
// table would survive far longer than the ten minutes the code is meant to live.
func (s *OTPService) record(ctx context.Context, action, addr string, challengeID *uuid.UUID) {
	target := addr
	if challengeID != nil {
		target = challengeID.String()
	}
	if err := s.audit.Record(ctx, audit.Entry{
		Action:     action,
		TargetType: "auth",
		TargetID:   target,
		Diff:       map[string]any{"email": addr},
	}); err != nil {
		s.log.WarnContext(ctx, "audit write failed", slog.String("action", action))
	}
}

// looksLikeEmail is a shape check, not a validity check. Deliverability is
// proven by the code arriving, so anything stricter would reject real
// addresses for no gain.
func looksLikeEmail(s string) bool {
	at := strings.Index(s, "@")
	if at <= 0 || at == len(s)-1 || strings.Count(s, "@") != 1 {
		return false
	}
	domain := s[at+1:]
	if !strings.Contains(domain, ".") || strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
		return false
	}
	return !strings.ContainsAny(s, " \t\r\n")
}
