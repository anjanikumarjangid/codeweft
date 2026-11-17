package ratelimit

import (
	"sync"
	"time"
)

// TokenBucket is a thread-safe token bucket rate limiter.
type TokenBucket[K comparable] struct {
	mu       sync.Mutex
	capacity int
	tokens   map[K]float64
	last     map[K]time.Time
	rate     float64 // tokens per second
}

// Clock lets tests control time.
type Clock = func() time.Time

// Allow reports whether key may proceed, consuming one token.
func (b *TokenBucket[K]) Allow(key K) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked(key)
	return b.tokens[key] >= 1
}

func (b *TokenBucket[K]) refillLocked(key K) {
	now := time.Now()
	b.tokens[key] = min(float64(b.capacity), refill(b.tokens[key], b.rate, now.Sub(b.last[key])))
	b.last[key] = now
}

func refill(current, rate float64, elapsed time.Duration) float64 {
	return current + rate*elapsed.Seconds()
}
