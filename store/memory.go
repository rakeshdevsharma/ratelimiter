package store

import (
	"sync"
	"hash/fnv"
)


const shards int = 1 << 4
// memoryStore is an in-memory Store. Entries are never evicted, so memory
// grows with every distinct key (see README, Known issues).
type memoryStore[T any] struct {
	shards [shards]*shard[T] 
}

type shard[T any] struct {
	mu   sync.RWMutex
	data map[string]*T
}

func NewMemoryStore[T any]() Store[T] {
	var s [shards]*shard[T]

	for i:=0;i<shards;i++ {
		s[i] = &shard[T] {
			data: make(map[string]*T),
			mu: sync.RWMutex{},
		}
	}
	return &memoryStore[T]{
		shards: s,
	}
}

func (s *memoryStore[T]) Get(key string) (*T, error) {
	bucket, err := s.bucket(key)
	if err != nil {
		return nil, err
	}
	bucket.mu.RLock()
	v, ok := bucket.data[key]
	bucket.mu.RUnlock()
	if ok {
		return v, nil
	}

	bucket.mu.Lock()
	defer bucket.mu.Unlock()
	// Another goroutine may have created it between RUnlock and Lock.
	if v, ok := bucket.data[key]; ok {
		return v, nil
	}
	v = new(T)
	bucket.data[key] = v
	return v, nil
}

func (s *memoryStore[T]) hashFNV(ipStr string) (uint64, error) {

	// Initialize the 64-bit FNV-1a hasher
	h := fnv.New64a()
	
	// Write the normalized bytes into the hash state
	h.Write([]byte(ipStr))
	
	return h.Sum64(), nil
}

func (s *memoryStore[T]) bucket(ipStr string) (*shard[T],  error) {
	h, err := s.hashFNV(ipStr)
	if err != nil {
		return nil, err
	}

	i := h & (uint64(shards) - 1)
	return s.shards[i], nil
}