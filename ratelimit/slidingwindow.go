package ratelimit

import (
	"net/http"
	"time"

	"study/ratelimiter/store"
)

// SlidingWindow: at most cfg.Limit requests per IP in any rolling cfg.Window.
// Two common variants:
//   - log: keep a timestamp per request, drop ones older than Window (exact,
//     more memory; RateLimitMeta would need a slice field).
//   - counter: weight the previous fixed window by its overlap with the
//     rolling window, e.g. prev*(1-elapsed/Window) + curr (approximate, O(1)).
//
// Natural fields (counter variant): Count = current window count,
// LastRefreshTS = current window start; you will also need the previous count.
type SlidingWindow struct {
	store store.Store[RateLimitMeta]
	cfg   Config
}

func NewSlidingWindow(st store.Store[RateLimitMeta], cfg Config) RateLimiter {
	return &SlidingWindow{store: st, cfg: cfg}
}

func (l *SlidingWindow) Check(ip string) int {
	meta, err := l.store.Get(ip)
	if err != nil {
		return http.StatusInternalServerError
	}
	meta.mu.Lock()
	defer meta.mu.Unlock()
	t := time.Now()

	lw := t.Truncate(l.cfg.Window)
	lrw := meta.LastRequestTS.Truncate(l.cfg.Window)
	ew := lw.Sub(lrw) / l.cfg.Window

	if ew >= 2 {
		meta.LastCount = 0
		meta.Count = 0
	} else if ew >= 1 {
		meta.LastCount = meta.Count
		meta.Count = 0
	}

	// Duration spent in current window
	x := t.Sub(lw)

	// remaining window
	y := l.cfg.Window - x

	if int(float64(y)/float64(l.cfg.Window)*meta.LastCount)+int(meta.Count) >= l.cfg.Limit {
		return http.StatusTooManyRequests
	}

	meta.Count = meta.Count + 1
	meta.LastRequestTS = t
	return http.StatusOK
}
