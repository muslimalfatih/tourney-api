// Package email delivers transactional mail. It is delivery ONLY: code
// generation, hashing, expiry, attempt counting, rate limiting and session
// issuance all live in internal/auth. Swapping providers must never change who
// can sign in.
package email

import (
	"context"
	"fmt"
	"sync"
)

// Sender delivers a message. Implementations must never return provider
// response bodies to callers — a delivery failure is a delivery failure, and
// leaking the provider's payload into an API response or a log is how API keys
// and recipient lists escape.
type Sender interface {
	SendOTP(ctx context.Context, to, code string) error
	SendInvitation(ctx context.Context, to, role string) error
}

// Message is one delivered email, recorded by FakeSender.
type Message struct {
	Kind string // "otp" | "invitation"
	To   string
	Code string // OTP only
	Role string // invitation only
}

// FakeSender records messages instead of sending them. Used by every automated
// test: unit, integration and browser. Nothing in the test suite may reach a
// real provider, both because tests must not email real people and because a
// test that needs network access stops being a test of this code.
type FakeSender struct {
	mu   sync.Mutex
	sent []Message

	// FailNext makes the next send return an error, for exercising the paths
	// that run when a provider is down.
	FailNext bool
}

func NewFakeSender() *FakeSender { return &FakeSender{} }

func (f *FakeSender) SendOTP(_ context.Context, to, code string) error {
	return f.record(Message{Kind: "otp", To: to, Code: code})
}

func (f *FakeSender) SendInvitation(_ context.Context, to, role string) error {
	return f.record(Message{Kind: "invitation", To: to, Role: role})
}

func (f *FakeSender) record(m Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.FailNext {
		f.FailNext = false
		return fmt.Errorf("email delivery failed")
	}
	f.sent = append(f.sent, m)
	return nil
}

// Sent returns a copy of everything delivered so far.
func (f *FakeSender) Sent() []Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Message, len(f.sent))
	copy(out, f.sent)
	return out
}

// LastCodeFor returns the most recent OTP delivered to an address, which is
// how tests obtain a code without the engine ever exposing one.
func (f *FakeSender) LastCodeFor(to string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.sent) - 1; i >= 0; i-- {
		if f.sent[i].Kind == "otp" && f.sent[i].To == to {
			return f.sent[i].Code
		}
	}
	return ""
}

// Reset clears recorded messages between test cases.
func (f *FakeSender) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = nil
	f.FailNext = false
}
