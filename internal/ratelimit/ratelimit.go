package ratelimit

import (
	"context"
	"fmt"
	"sync"
	"time"

	"telegram-notifier/internal/constants"
)

// TokenBucket implements a token bucket rate limiter
type TokenBucket struct {
	tokens     float64
	maxTokens  float64
	refillRate float64
	lastRefill time.Time
	mu         sync.Mutex
}

// NewTokenBucket creates a new rate limiter
func NewTokenBucket(maxTokens int, refillRate time.Duration) *TokenBucket {
	return &TokenBucket{
		tokens:     float64(maxTokens),
		maxTokens:  float64(maxTokens),
		refillRate: 1.0 / refillRate.Seconds(),
		lastRefill: time.Now(),
	}
}

// Wait blocks until a token is available or context is cancelled
func (tb *TokenBucket) Wait(ctx context.Context) error {
	deadline := time.Now().Add(constants.RateLimitMaxWaitTime)

	for {
		if tb.tryTake() {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("rate limit wait cancelled: %w", ctx.Err())
		case <-time.After(100 * time.Millisecond):
			if time.Now().After(deadline) {
				return fmt.Errorf("rate limit wait timeout after %v", constants.RateLimitMaxWaitTime)
			}
		}
	}
}

// tryTake attempts to take a token, returns true if successful
func (tb *TokenBucket) tryTake() bool {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	tb.refill()

	if tb.tokens >= 1.0 {
		tb.tokens--
		return true
	}
	return false
}

// refill adds tokens based on time elapsed
func (tb *TokenBucket) refill() {
	now := time.Now()
	elapsed := now.Sub(tb.lastRefill).Seconds()
	tb.tokens += elapsed * tb.refillRate

	if tb.tokens > tb.maxTokens {
		tb.tokens = tb.maxTokens
	}

	tb.lastRefill = now
}
