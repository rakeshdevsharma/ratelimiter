package ratelimit

import (
	"sync"
	"time"
)

type RateLimiter interface {
	// Check returns 200 (allow), 429 (limited) or 500 when the store fails.
	// The handler passes 500 on to Nginx, which fails open if configured to.
	Check(ip string) int
}

// RateLimitMeta is the per-IP state kept in a store.Store[RateLimitMeta].
// Each strategy uses the fields it needs, under mu.
type RateLimitMeta struct {
	mu            sync.Mutex
	Count         float64
	LastCount     float64
	LastRequestTS time.Time
	LastRefreshTS time.Time
}
