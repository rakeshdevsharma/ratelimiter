package ratelimit

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"study/ratelimiter/store"
)

// Config holds generic knobs; each strategy reads the ones it needs.
type Config struct {
	Limit  int           // max requests (fixed/sliding window) or bucket capacity (token/leaky bucket)
	Window time.Duration // window length (fixed/sliding window); token/leaky bucket refill/drain at Limit/Window
}

// Validate rejects values the strategies can't handle: a zero Window divides
// by zero, and a zero Limit would reject every request.
func (c Config) Validate() error {
	if c.Limit <= 0 {
		return fmt.Errorf("limit must be > 0, got %d", c.Limit)
	}
	if c.Window <= 0 {
		return fmt.Errorf("window must be > 0, got %s", c.Window)
	}
	return nil
}

var strategies = map[string]func(store.Store[RateLimitMeta], Config) RateLimiter{
	"tokenbucket":   NewTokenBucket,
	"leakybucket":   NewLeakyBucket,
	"fixedwindow":   NewFixedWindow,
	"slidingwindow": NewSlidingWindow,
}

// Names returns the registered strategy names, sorted.
func Names() []string {
	names := make([]string, 0, len(strategies))
	for n := range strategies {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// New builds the strategy registered under name.
func New(name string, st store.Store[RateLimitMeta], cfg Config) (RateLimiter, error) {
	ctor, ok := strategies[name]
	if !ok {
		return nil, fmt.Errorf("unknown strategy %q (valid: %s)", name, strings.Join(Names(), ", "))
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	return ctor(st, cfg), nil
}
