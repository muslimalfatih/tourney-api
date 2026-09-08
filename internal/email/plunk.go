package email

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const plunkSendURL = "https://api.useplunk.com/v1/send"

// PlunkSender delivers through Plunk's transactional API.
//
// Delivery only. Nothing about who may sign in is decided here.
type PlunkSender struct {
	apiKey    string
	fromEmail string
	fromName  string
	client    *http.Client
}

// NewPlunkSender builds a real sender, or refuses.
//
// The guard is the point: outside production a real send is refused unless
// allowRealSend is explicitly set. A misconfigured test run should fail loudly
// rather than email a real organizer a code they did not ask for.
func NewPlunkSender(apiKey, fromEmail, fromName string, isProduction, allowRealSend bool) (*PlunkSender, error) {
	if !isProduction && !allowRealSend {
		return nil, fmt.Errorf(
			"refusing to build a real email sender outside production; " +
				"set PLUNK_ALLOW_REAL_SEND=true only if you mean to deliver real mail")
	}
	if apiKey == "" || fromEmail == "" {
		return nil, fmt.Errorf("PLUNK_API_KEY and PLUNK_FROM_EMAIL are required")
	}
	return &PlunkSender{
		apiKey:    apiKey,
		fromEmail: fromEmail,
		fromName:  fromName,
		// A hung provider must not hold an API request open indefinitely.
		client: &http.Client{Timeout: 10 * time.Second},
	}, nil
}

func (p *PlunkSender) SendOTP(ctx context.Context, to, code string) error {
	return p.send(ctx, to,
		"Your Tourney.social sign-in code",
		fmt.Sprintf(
			"Your sign-in code is: %s\n\n"+
				"This code expires in 10 minutes. If you did not request it, you can ignore\n"+
				"this email.\n\n"+
				"Do not forward this code to anyone.", code))
}

func (p *PlunkSender) SendInvitation(ctx context.Context, to, role string) error {
	return p.send(ctx, to,
		"You've been invited to Tourney.social",
		fmt.Sprintf(
			"You've been invited to access Tourney.social as an %s.\n\n"+
				"Use this email address to sign in:\n%s\n\n"+
				"When you sign in, we will send a one-time verification code to this address.",
			role, to))
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

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, plunkSendURL, bytes.NewReader(buf))
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
		return fmt.Errorf("email delivery failed with status %d", res.StatusCode)
	}
	return nil
}
