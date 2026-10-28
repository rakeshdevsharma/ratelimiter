// Package store holds per-key state for the rate limiter. It knows nothing
// about rate limiting: values are opaque *T owned by the caller.
package store

// Store maps a key (client IP) to a *T.
type Store[T any] interface {
	// Get returns the entry for key, creating a zero T if none exists.
	// Concurrent first calls for the same key get the same pointer.
	Get(key string) (*T, error)
}
