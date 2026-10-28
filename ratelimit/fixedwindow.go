package ratelimit

import (
	"net/http"
	"time"

	"study/ratelimiter/store"
)

// FixedWindow: time is cut into windows of cfg.Window; at most cfg.Limit
// requests per IP per window. The counter resets when a new window starts.
// Watch for the boundary burst: up to 2x Limit across two adjacent windows.
//
// Natural fields: Count = requests in the current window, LastRefreshTS =
// start of the current window.
type FixedWindow struct {
	store store.Store[RateLimitMeta]
	cfg   Config
}

func NewFixedWindow(st store.Store[RateLimitMeta], cfg Config) RateLimiter {
	return &FixedWindow{store: st, cfg: cfg}
}

func (l *FixedWindow) Check(ip string) int {
	meta, err := l.store.Get(ip)
	if err != nil {
		return http.StatusInternalServerError
	}
	meta.mu.Lock()
	defer meta.mu.Unlock()
	t := time.Now()
	if t.Sub(meta.LastRefreshTS) >= l.cfg.Window {
		meta.Count = float64(l.cfg.Limit)
		meta.LastRefreshTS = t
	}

	if meta.Count > 0 {
		meta.Count -= 1
		return http.StatusOK
	} else {
		return http.StatusTooManyRequests
	}
}
