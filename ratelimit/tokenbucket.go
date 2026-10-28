package ratelimit

import (
	"net/http"
	"time"

	"study/ratelimiter/store"
)

// 1. Each request takes one token.
// 2. Burst upto Config.Limit are allowed.
// 3. Once the burst capacity 'Config.Limit' is exhausted in the given 'cfg.Window', new tokens are issued at the rate derived
// by  Config.Limit/cfg.Window
// 4. The Algorithm calculates how many tokens should have been issued, since the last time of issue and those many tokens are
// issued.
// Pros:
// 1. Bucket is refilled only when all tokens are used, so for a regular client no recalculation is needed per request,
// only the counter needs to be decremented.
// 2. Burst Capacity is configurable.
// 3.
type TokenBucket struct {
	store store.Store[RateLimitMeta]
	cfg   Config
}

func NewTokenBucket(st store.Store[RateLimitMeta], cfg Config) RateLimiter {
	return &TokenBucket{store: st, cfg: cfg}
}

func (l *TokenBucket) Check(ip string) int {
	meta, err := l.store.Get(ip)
	if err != nil {
		return http.StatusInternalServerError
	}
	meta.mu.Lock()
	defer meta.mu.Unlock()
	t := time.Now()
	l.refill(meta, t)

	if int(meta.Count) > 0 {
		meta.Count -= 1
	} else {
		return http.StatusTooManyRequests
	}
	return http.StatusOK
}

func (l *TokenBucket) refill(meta *RateLimitMeta, t time.Time) {
	elapsed := t.Sub(meta.LastRefreshTS)

	// Calculate what percent of the window has elapsed since last refresh. Add that much percent of the tokens.
	// In other words, Add fraction of tokens which should have been allotted in this fraction of window.
	meta.Count += (elapsed.Seconds() / l.cfg.Window.Seconds()) * float64(l.cfg.Limit)

	if meta.Count > float64(l.cfg.Limit) {
		meta.Count = float64(l.cfg.Limit)
	}
	meta.LastRefreshTS = t

}
