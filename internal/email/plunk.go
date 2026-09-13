package email

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultPlunkSendURL is Plunk's transactional endpoint.
//
// Plunk's own docs show BOTH api.useplunk.com and next-api.useplunk.com
// depending on which page you land on. This used to default to the former,
// which cost a live incident: a valid key posted there comes back
//
//	401 {"error":"Unauthorized","message":"Incorrect Bearer token specified"}
//
// while the identical request to next-api succeeds. The failure reads exactly
// like a bad key, so the first instinct is to go rotate a key that was fine
// all along. Verified by hand against both hosts on 2026-09-13.
//
// Still overridable with PLUNK_API_URL, since only Plunk knows when this moves.
const DefaultPlunkSendURL = "https://next-api.useplunk.com/v1/send"

// PlunkSender delivers through Plunk's transactional API.
//
// Delivery only. Nothing about who may sign in is decided here.
type PlunkSender struct {
	apiKey    string
	fromEmail string
	fromName  string
	sendURL   string
	client    *http.Client
}

// NewPlunkSender builds a real sender, or refuses.
//
// The guard is the point: outside production a real send is refused unless
// allowRealSend is explicitly set. A misconfigured test run should fail loudly
// rather than email a real organizer a code they did not ask for.
func NewPlunkSender(apiKey, fromEmail, fromName, sendURL string, isProduction, allowRealSend bool) (*PlunkSender, error) {
	if !isProduction && !allowRealSend {
		return nil, fmt.Errorf(
			"refusing to build a real email sender outside production; " +
				"set PLUNK_ALLOW_REAL_SEND=true only if you mean to deliver real mail")
	}
	if apiKey == "" || fromEmail == "" {
		return nil, fmt.Errorf("PLUNK_API_KEY and PLUNK_FROM_EMAIL are required")
	}
	if sendURL == "" {
		sendURL = DefaultPlunkSendURL
	}
	return &PlunkSender{
		apiKey:    apiKey,
		fromEmail: fromEmail,
		fromName:  fromName,
		sendURL:   sendURL,
		// A hung provider must not hold an API request open indefinitely.
		client: &http.Client{Timeout: 10 * time.Second},
	}, nil
}

// Email is a hostile medium, so these templates break several rules the web app
// follows, deliberately:
//
//   - Tables, not flexbox. Outlook renders through Word, which has no flex.
//   - Inline styles, not a <style> block. Plunk wraps this HTML in its own
//     document, and several clients strip <style> outright.
//   - No CSS variables and no web fonts. The theme's Instrument Serif will
//     never load here, so the frame names Georgia directly — which is already
//     the fallback layout.css declares, so the two stay in agreement.
//   - Hardcoded hex, kept in sync with tourney-web's layout.css by hand.
//
// shell wraps body copy in that frame so every message this package sends looks
// like one product rather than two.
func shell(inner string) string {
	return `<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" ` +
		`style="background-color:#0d0d0f;margin:0;padding:32px 16px;width:100%">` +
		`<tr><td align="center">` +
		`<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" ` +
		`style="max-width:460px;background-color:#151517;border:1px solid #2b2b2f;border-radius:14px">` +
		`<tr><td style="padding:26px 28px 0">` +
		// The wordmark is text, not the app's SVG: Gmail strips inline SVG, and
		// a broken image is worse than no logo at all.
		`<div style="font-family:Georgia,'Times New Roman',serif;font-size:17px;color:#d4a94e;` +
		`letter-spacing:-0.01em">tourney<span style="color:#8f8c84">.social</span></div>` +
		`</td></tr>` +
		`<tr><td style="padding:18px 28px 28px;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,sans-serif">` +
		inner +
		`</td></tr></table>` +
		`<div style="max-width:460px;margin:16px auto 0;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,sans-serif;` +
		`font-size:11px;line-height:1.5;color:#6f6c66;text-align:center">` +
		`Tourney.social is invitation-only. There is no public sign-up.</div>` +
		`</td></tr></table>`
}

// article turns a role identifier into the phrase a person reads. The previous
// wording interpolated the raw value after a fixed "an", so a super admin was
// invited "as an super_admin" — wrong article, and an internal identifier
// shown to someone who has no reason to know the database uses one.
func article(role string) string {
	switch role {
	case "super_admin":
		return "a super admin"
	case "organizer":
		return "an organizer"
	default:
		return "a " + strings.ReplaceAll(role, "_", " ")
	}
}

// Bodies are pure functions of their inputs, separate from delivery, so the
// rendered markup can be asserted and eyeballed without a network call — and
// without anything reaching a real inbox.
func otpBody(code string) string {
	return fmt.Sprintf(
		`<div style="font-family:Georgia,'Times New Roman',serif;font-size:22px;color:#ece9e2;margin:0 0 6px">Your sign-in code</div>`+
			`<div style="font-size:14px;line-height:1.55;color:#8f8c84;margin:0 0 20px">Enter it on the sign-in screen to continue.</div>`+
			// The code sits in its own panel so it reads as a token to copy,
			// not as a heading. letter-spacing is presentation only — it does
			// not travel with the text when the recipient copies it.
			`<div style="background-color:#1e1e21;border:1px solid #2b2b2f;border-radius:10px;padding:18px 20px;text-align:center">`+
			`<div style="font-family:ui-monospace,SFMono-Regular,'SF Mono',Consolas,'Liberation Mono',monospace;`+
			`font-size:30px;font-weight:600;letter-spacing:0.28em;color:#ece9e2;`+
			// text-indent offsets the trailing letter-space so the digits sit
			// optically centred rather than pushed left.
			`text-indent:0.28em;line-height:1.2">%s</div>`+
			`</div>`+
			`<div style="font-size:13px;line-height:1.6;color:#8f8c84;margin:20px 0 0">`+
			`Expires in 10 minutes. If you did not request it, you can ignore this email.</div>`+
			`<div style="font-size:13px;line-height:1.6;color:#d4a94e;margin:10px 0 0">`+
			`Do not forward this code to anyone.</div>`,
		html.EscapeString(code))
}

func (p *PlunkSender) SendOTP(ctx context.Context, to, code string) error {
	return p.send(ctx, to, "Your Tourney.social sign-in code", shell(otpBody(code)))
}

func invitationBody(to, role string) string {
	return fmt.Sprintf(
		`<div style="font-family:Georgia,'Times New Roman',serif;font-size:22px;color:#ece9e2;margin:0 0 6px">You've been invited</div>`+
			`<div style="font-size:14px;line-height:1.55;color:#8f8c84;margin:0 0 20px">`+
			`Your account on Tourney.social is ready, as %s.</div>`+
			`<div style="background-color:#1e1e21;border:1px solid #2b2b2f;border-radius:10px;padding:16px 20px">`+
			`<div style="font-size:11px;text-transform:uppercase;letter-spacing:0.14em;color:#6f6c66;margin:0 0 6px">Sign in with</div>`+
			`<div style="font-family:ui-monospace,SFMono-Regular,'SF Mono',Consolas,'Liberation Mono',monospace;`+
			// Long addresses must wrap rather than widen the table and force
			// the whole message to scroll sideways on a phone.
			`font-size:15px;color:#ece9e2;word-break:break-all">%s</div>`+
			`</div>`+
			`<div style="font-size:13px;line-height:1.6;color:#8f8c84;margin:20px 0 0">`+
			`There is no password. Each time you sign in, we email a six-digit code to this address.</div>`,
		html.EscapeString(article(role)), html.EscapeString(to))
}

func (p *PlunkSender) SendInvitation(ctx context.Context, to, role string) error {
	return p.send(ctx, to, "You've been invited to Tourney.social", shell(invitationBody(to, role)))
}

func (p *PlunkSender) send(ctx context.Context, to, subject, body string) error {
	payload := map[string]any{
		"to":         to,
		"subject":    subject,
		"body":       body,
		"from":       p.fromEmail,
		"name":       p.fromName,
		"subscribed": false,
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.sendURL, bytes.NewReader(buf))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	req.Header.Set("Content-Type", "application/json")

	res, err := p.client.Do(req)
	if err != nil {
		// Deliberately does not wrap err: a transport error can include the
		// full request URL, and callers log what they are handed.
		return fmt.Errorf("email delivery failed")
	}
	defer res.Body.Close()
	// Drain so the connection can be reused, but never keep the body: it can
	// echo the recipient and, on some providers, the key.
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return &DeliveryError{Status: res.StatusCode}
	}
	return nil
}

// DeliveryError reports that the provider rejected a send, carrying ONLY the
// HTTP status. That is the one detail an operator needs and the one detail
// that cannot leak anything: the response body may echo the recipient or the
// key, so it is never captured. Callers can log Status without having to
// decide, at the log site, whether the text they were handed is safe.
type DeliveryError struct{ Status int }

func (e *DeliveryError) Error() string {
	return fmt.Sprintf("email delivery failed with status %d", e.Status)
}
