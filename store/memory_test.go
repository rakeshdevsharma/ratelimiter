package store

import (
	"sync"
	"testing"
)

func TestGetConcurrentFirstCallsShareEntry(t *testing.T) {
	s := NewMemoryStore[int]()
	const n = 100
	got := make([]*int, n)
	errs := make([]error, n)
	var start, done sync.WaitGroup
	start.Add(1)
	for i := range n {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			got[i], errs[i] = s.Get("10.0.0.1")
		}()
	}
	start.Done()
	done.Wait()
	for i, p := range got {
		if errs[i] != nil || p == nil {
			t.Fatalf("Get #%d: %v, %v", i, p, errs[i])
		}
		if p != got[0] {
			t.Fatalf("Get #%d returned a different entry", i)
		}
	}
}
