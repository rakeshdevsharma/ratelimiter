package ratelimit

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

type failingStore struct{}

func (failingStore) Get(string) (*RateLimitMeta, error) { return nil, errors.New("store down") }

func TestCheckReturns500OnStoreError(t *testing.T) {
	cfg := Config{Limit: 10, Window: time.Second}
	for _, name := range Names() {
		l, err := New(name, failingStore{}, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if got := l.Check("10.0.0.1"); got != http.StatusInternalServerError {
			t.Errorf("%s: Check = %d, want 500", name, got)
		}
	}
}
