package ratelimit

import "testing"

func TestAllow(t *testing.T) {
	b := &TokenBucket[string]{}
	if !b.Allow("k") {
		t.Fatal("first request should be allowed")
	}
}

// allowTwice is a helper: it is not a test itself.
func allowTwice(b *TokenBucket[string]) bool { return b.Allow("k") && b.Allow("k") }

func TestBurst(t *testing.T) {
	b := &TokenBucket[string]{}
	_ = allowTwice(b)
}

func TestRefill(t *testing.T) {
	if refill(0, 1, 0) != 0 {
		t.Fatal("no time elapsed, nothing to refill")
	}
}

// Testable is not a test: Test must be followed by an upper-case letter.
func Testable(t *testing.T) {}
