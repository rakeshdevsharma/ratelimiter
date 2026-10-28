package ratelimit

import (
	"net/http"
	"time"

	"study/ratelimiter/store"
)

// LeakyBucket: each IP has a queue/"water level" of capacity cfg.Limit that
// drains at cfg.Limit/cfg.Window requests/second. A request adds one unit; if the bucket
// would overflow, 429. Output is smoothed to a steady rate.
//
// Natural fields: Count = current level, LastRefreshTS = when the level was
// last drained (leak lazily from the elapsed time on each request).
type LeakyBucket struct {
	store store.Store[RateLimitMeta]
	cfg   Config
}

func NewLeakyBucket(st store.Store[RateLimitMeta], cfg Config) RateLimiter {
	return &LeakyBucket{store: st, cfg: cfg}
}

func (l *LeakyBucket) Check(ip string) int {
	meta, err := l.store.Get(ip)
	if err != nil {
		return http.StatusInternalServerError
	}
	meta.mu.Lock()
	defer meta.mu.Unlock()
	t := time.Now()
	l.leak(meta, t)

	if meta.Count+1 <= float64(l.cfg.Limit) {
		meta.Count += 1
		return http.StatusOK
	} else {
		return http.StatusTooManyRequests
	}
}

func (l *LeakyBucket) leak(meta *RateLimitMeta, t time.Time) {
	elapsed := t.Sub(meta.LastRefreshTS)

	// Calculate what percent of the window has elapsed since last refresh. Deduct that much percent of the tokens.
	// In other words, deduct fraction of tokens which should have been allotted in this fraction of window.
	meta.Count -= (elapsed.Seconds() / l.cfg.Window.Seconds()) * float64(l.cfg.Limit)

	if meta.Count < 0 {
		meta.Count = 0
	}

	meta.LastRefreshTS = t
}
