package store

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type memoryEntry struct {
	value int64
	expiresAt time.Time
}


// shard reduces lock contention — instead of one mutex for all keys,
// we have 64 independent shards each protecting a subset of keys.
const numShards = 64

type Shard struct {
	mu sync.RWMutex
	data map[string]memoryEntry
}

type MemoryStore struct {
	shards [numShards]Shard
}


func NewMemoryStore() *MemoryStore {
	m := &MemoryStore{}
	for i := range m.shards {
		m.shards[i].data = make(map[string]memoryEntry)
	}
	return m
}

// getShard picks the shard for a key using a simple hash.
func (m *MemoryStore) getShard(key string) *Shard {
	hash := 0
	for _, c := range key {
		hash = (hash*31 + int(c)) % numShards
	}
	return &m.shards[hash]
}

func (m *MemoryStore) Get(ctx context.Context, key string) (int64, error){
	s := m.getShard(key)
	s.mu.RLock()
	defer s.mu.RUnlock()

	entry, ok := s.data[key]
	if !ok || time.Now().After(entry.expiresAt){
		return 0, nil
	}

	return entry.value, nil
}

func (m *MemoryStore) Set(ctx context.Context, key string, value int64, ttl time.Duration)  error{
	s := m.getShard(key)
	s.mu.Lock()
	defer s.mu.Unlock()

	s.data[key] = memoryEntry{
		value: value,
		expiresAt: time.Now().Add(ttl),
	}

	return nil
}

func (m *MemoryStore) Increment(ctx context.Context, key string, ttl time.Duration) (int64, error){
	s := m.getShard(key)
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.data[key]
	if !ok || time.Now().After(entry.expiresAt) {
		// key is new or expired — start fresh
		s.data[key] = memoryEntry{
			value: 1,
			expiresAt: time.Now().Add(ttl),
		}
		return 1, nil
	}

	entry.value++
	s.data[key] = entry
	return entry.value, nil
}


// Eval is a no-op stub for the in-memory store.
// Each algorithm that uses Lua in Redis has an equivalent
// native Go implementation used when MemoryStore is active.
func (m *MemoryStore) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error){
	return nil, fmt.Errorf("Eval not supported for memory store - use native implementation")
}

func(m *MemoryStore) Ping(ctx context.Context) error {
	return nil
}
