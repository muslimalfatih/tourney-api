package email

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
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

// Plunk renders `body` as HTML, so newlines alone would collapse into one
// paragraph. The code is wrapped in a <strong> and spaced out, since the whole
// job of this email is to make six digits easy to read off a phone.
func (p *PlunkSender) SendOTP(ctx context.Context, to, code string) error {
	return p.send(ctx, to,
		"Your Tourney.social sign-in code",
		fmt.Sprintf(
			`<p>Your sign-in code is:</p>`+
				`<p style="font-size:28px;letter-spacing:6px;font-weight:700;margin:16px 0">%s</p>`+
				`<p>This code expires in 10 minutes. If you did not request it, you can ignore this email.</p>`+
				`<p>Do not forward this code to anyone.</p>`,
			html.EscapeString(code)))
}

func (p *PlunkSender) SendInvitation(ctx context.Context, to, role string) error {
	return p.send(ctx, to,
		"You've been invited to Tourney.social",
		fmt.Sprintf(
			`<p>You've been invited to access Tourney.social as an %s.</p>`+
				`<p>Use this email address to sign in:<br><strong>%s</strong></p>`+
				`<p>When you sign in, we will send a one-time verification code to this address.</p>`,
			html.EscapeString(role), html.EscapeString(to)))
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
