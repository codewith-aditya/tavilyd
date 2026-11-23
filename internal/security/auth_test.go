package security

import (
	"testing"
	"time"
)

func TestAuthenticatorDisabledWithoutKeys(t *testing.T) {
	a := NewAuthenticator(nil)
	if a.Enabled() {
		t.Fatal("authenticator should be disabled")
	}
	if !a.Check("") || !a.Check("anything") {
		t.Fatal("disabled authenticator must allow everything")
	}
}

func TestAuthenticatorConstantTimeCompare(t *testing.T) {
	a := NewAuthenticator([]string{" key-one ", "key-two"})
	if !a.Enabled() {
		t.Fatal("expected auth enabled")
	}
	if !a.Check("key-one") || !a.Check("key-two") {
		t.Fatal("valid keys rejected")
	}
	if a.Check("key-on") || a.Check("key-one ") || a.Check("") {
		t.Fatal("invalid keys accepted")
	}
}

func TestAuthenticatorTrimsConfiguredKeys(t *testing.T) {
	a := NewAuthenticator([]string{"", "  ", "secret"})
	if len(a.hashes) != 1 {
		t.Fatalf("expected one usable key, got %d", len(a.hashes))
	}
	if !a.Check("secret") {
		t.Fatal("trimmed key rejected")
	}
}

func TestRateLimiterBurstThenRefusal(t *testing.T) {
	r := NewRateLimiter(0.001, 2, time.Minute)
	now := time.Unix(1700000000, 0)
	r.now = func() time.Time { return now }

	ok, _ := r.Allow("k")
	if !ok {
		t.Fatal("first request should pass")
	}
	ok, _ = r.Allow("k")
	if !ok {
		t.Fatal("second request should pass")
	}
	ok, retry := r.Allow("k")
	if ok {
		t.Fatal("third request should be limited")
	}
	if retry <= 0 {
		t.Fatalf("expected a positive retry hint, got %v", retry)
	}
	// A different key has its own bucket.
	if ok, _ := r.Allow("other"); !ok {
		t.Fatal("independent key should not be limited")
	}
}

func TestRateLimiterDisabled(t *testing.T) {
	r := NewRateLimiter(0, 1, time.Minute)
	for i := 0; i < 100; i++ {
		if ok, _ := r.Allow("k"); !ok {
			t.Fatal("disabled limiter must always allow")
		}
	}
}

func TestRateLimiterPrunesIdleBuckets(t *testing.T) {
	r := NewRateLimiter(100, 10, time.Minute)
	now := time.Unix(1700000000, 0)
	r.now = func() time.Time { return now }
	r.Allow("a")
	r.Allow("b")
	now = now.Add(2 * time.Minute)
	r.Allow("c")
	r.mu.Lock()
	n := len(r.buckets)
	r.mu.Unlock()
	if n > 2 {
		t.Fatalf("idle buckets not pruned: %d", n)
	}
}
