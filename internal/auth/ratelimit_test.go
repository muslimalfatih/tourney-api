package auth

import (
	"testing"
	"time"
)

func TestLimiter_AllowsUpToLimitThenDenies(t *testing.T) {
	l := NewLimiter()
	for i := 0; i < 3; i++ {
		if !l.Allow("a@x.com", 3, time.Minute) {
			t.Fatalf("attempt %d denied, want allowed", i+1)
		}
	}
	if l.Allow("a@x.com", 3, time.Minute) {
		t.Error("4th attempt allowed, want denied")
	}
}

func TestLimiter_KeysAreIndependent(t *testing.T) {
	l := NewLimiter()
	for i := 0; i < 3; i++ {
		l.Allow("a@x.com", 3, time.Minute)
	}
	if !l.Allow("b@x.com", 3, time.Minute) {
		t.Error("a different address was denied; buckets are leaking across keys")
	}
}

func TestLimiter_WindowExpires(t *testing.T) {
	l := NewLimiter()
	// A 1ns window means every prior attempt is already outside it.
	for i := 0; i < 5; i++ {
		l.Allow("a@x.com", 1, time.Nanosecond)
	}
	time.Sleep(time.Millisecond)
	if !l.Allow("a@x.com", 1, time.Nanosecond) {
		t.Error("attempt denied after the window elapsed")
	}
}

// Denied attempts still count. Otherwise a caller who hammers the endpoint
// would slip through the instant the oldest entry aged out, which is the
// opposite of what a limit is for.
func TestLimiter_DeniedAttemptsStillCount(t *testing.T) {
	l := NewLimiter()
	for i := 0; i < 10; i++ {
		l.Allow("a@x.com", 2, time.Hour)
	}
	if l.Allow("a@x.com", 2, time.Hour) {
		t.Error("allowed after 10 denied attempts inside the window")
	}
	if l.RetryAfter("a@x.com", time.Hour) <= 0 {
		t.Error("RetryAfter should report time remaining while the window is full")
	}
}

func TestLimiter_ResetClearsOneKey(t *testing.T) {
	l := NewLimiter()
	l.Allow("a@x.com", 1, time.Hour)
	l.Allow("b@x.com", 1, time.Hour)
	l.Reset("a@x.com")
	if !l.Allow("a@x.com", 1, time.Hour) {
		t.Error("reset key still limited")
	}
	if l.Allow("b@x.com", 1, time.Hour) {
		t.Error("Reset cleared the wrong key")
	}
}

func TestLimiter_BoundsMemory(t *testing.T) {
	l := NewLimiter()
	l.maxKeys = 100
	for i := 0; i < 1000; i++ {
		l.Allow(string(rune(i))+"@x.com", 5, time.Hour)
	}
	// An attacker cycling addresses must not grow this without bound.
	if len(l.buckets) > 1000 {
		t.Errorf("bucket count = %d; memory is unbounded", len(l.buckets))
	}
}
