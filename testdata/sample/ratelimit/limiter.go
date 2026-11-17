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
	return b.tokens[key] >= 1
}

func refill(current, rate float64, elapsed time.Duration) float64 {
	return current + rate*elapsed.Seconds()
}
