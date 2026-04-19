// ratelimiter.go — ProViber Rate Limiter
//
// A simple token-bucket rate limiter keyed by client IP address.
// Each IP gets a bucket that refills at a fixed rate (e.g., 5 tokens per minute).
//
// Purpose: protect the free-tier Gemini API from being exhausted by
// a single user sending too many requests in a short window.

package main

import (
	"sync"
	"time"
)

// ─────────────────────────────────────────────────────────────
// TOKEN BUCKET ENTRY — one per unique client IP
// ─────────────────────────────────────────────────────────────

type bucket struct {
	tokens    float64   // current token count (fractional for smooth refill)
	lastRefil time.Time // when the bucket was last refilled
}

// ─────────────────────────────────────────────────────────────
// RATE LIMITER
// ─────────────────────────────────────────────────────────────

// RateLimiter enforces a maximum number of requests per time window per IP.
type RateLimiter struct {
	mu       sync.Mutex
	buckets  map[string]*bucket
	maxBurst float64       // maximum tokens a bucket can hold (== requests allowed in a burst)
	refillAt float64       // tokens added per second
	window   time.Duration // convenience: for display / logging only

	// Background cleanup
	cleanupInterval time.Duration
	stopCleanup     chan struct{}
}

// NewRateLimiter creates a limiter allowing maxRequests per window, per IP.
//
// Example: NewRateLimiter(5, time.Minute)
//
//	→ 5 requests / minute / IP
//	→ token refill rate: 5/60 ≈ 0.0833 tokens/second
func NewRateLimiter(maxRequests int, window time.Duration) *RateLimiter {
	rl := &RateLimiter{
		buckets:         make(map[string]*bucket),
		maxBurst:        float64(maxRequests),
		refillAt:        float64(maxRequests) / window.Seconds(),
		window:          window,
		cleanupInterval: 5 * time.Minute,
		stopCleanup:     make(chan struct{}),
	}
	go rl.cleanupLoop()
	return rl
}

// Allow returns true if the given key (IP address) is within the rate limit.
// It atomically consumes one token from the bucket.
func (rl *RateLimiter) Allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	b, exists := rl.buckets[key]

	if !exists {
		// First request from this IP — start with a full bucket
		rl.buckets[key] = &bucket{
			tokens:    rl.maxBurst - 1, // consume one token immediately
			lastRefil: now,
		}
		return true
	}

	// Refill tokens based on elapsed time since last request
	elapsed := now.Sub(b.lastRefil).Seconds()
	b.tokens += elapsed * rl.refillAt
	if b.tokens > rl.maxBurst {
		b.tokens = rl.maxBurst // cap at max burst
	}
	b.lastRefil = now

	if b.tokens < 1 {
		// Not enough tokens — request is rejected
		return false
	}

	// Consume one token and allow the request
	b.tokens--
	return true
}

// Remaining returns the current token count for an IP (useful for headers).
func (rl *RateLimiter) Remaining(key string) int {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	b, ok := rl.buckets[key]
	if !ok {
		return int(rl.maxBurst)
	}
	return int(b.tokens)
}

// Reset clears all rate-limit state for a given key (admin/test use).
func (rl *RateLimiter) Reset(key string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	delete(rl.buckets, key)
}

// Stop shuts down the background cleanup goroutine.
func (rl *RateLimiter) Stop() {
	close(rl.stopCleanup)
}

// ─────────────────────────────────────────────────────────────
// BACKGROUND CLEANUP — removes stale buckets to prevent memory leaks
// ─────────────────────────────────────────────────────────────

// cleanupLoop periodically removes buckets for IPs that have been
// inactive longer than 2× the rate-limit window.
func (rl *RateLimiter) cleanupLoop() {
	ticker := time.NewTicker(rl.cleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-rl.stopCleanup:
			return
		case <-ticker.C:
			rl.cleanup()
		}
	}
}

func (rl *RateLimiter) cleanup() {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	cutoff := time.Now().Add(-2 * rl.window)
	for key, b := range rl.buckets {
		if b.lastRefil.Before(cutoff) {
			delete(rl.buckets, key)
		}
	}
}
