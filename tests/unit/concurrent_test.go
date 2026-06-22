package unit

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sharma-ayush1999/go-ratelimiter/internal/algorithm"
	"github.com/sharma-ayush1999/go-ratelimiter/internal/store"
	"github.com/stretchr/testify/assert"
)

// concurrentAllow fires n goroutines simultaneously against limiter
// and returns how many were allowed vs denied.
func concurrentAllow(t *testing.T, limiter algorithm.RateLimiter, key string, n int) (allowed, denied int64){
	t.Helper()
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(){
			defer wg.Done()
			status, err := limiter.Allow(context.Background(), key)
			if err != nil {
				return
			}
			if status.Allowed {
				atomic.AddInt64(&allowed, 1)
			} else {
				atomic.AddInt64(&denied, 1)
			}
		}()
	}
	wg.Wait()
	return allowed, denied
}

func TestFixedWindow_Concurrent(t *testing.T) {
	const limit = 100
	s := store.NewMemoryStore()
	limiter := algorithm.NewFixedWindow(s, algorithm.FixedWindowConfig{
		Limit: limit,
		Window: time.Minute,
	})

	allowed, denied := concurrentAllow(t, limiter, "concurrent-fw", 200)

	assert.Equal(t, int64(limit), allowed, "exactly %d requests should be allowed", limit)
	assert.Equal(t, int64(100), denied, "exactly 100 requests should be allowed")
}

func TestSlidingWindowCounter_Concurrent(t *testing.T) {
	const limit = 50
	s := store.NewMemoryStore()
	limiter := algorithm.NewSlidingWindowCounter(s, algorithm.SlidingWindowCounterConfig{
		Limit: limit,
		Window: time.Minute,
	})

	allowed, denied := concurrentAllow(t, limiter, "concurrent-swc", 150)

	assert.Equal(t, int64(limit), allowed)
	assert.Equal(t, int64(100), denied)
}

func TestMemoryStore_Concurrent_No(t *testing.T) {
	// This test is designed to be run with: go test -race
	// It verifies the sharded mutex correctly prevents data races.
	s := store.NewMemoryStore()

	var wg sync.WaitGroup
	for i:= 0; i < 500; i++{
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			key := "race-key"
			_, _ = s.Increment(context.Background(), key, time.Minute)
			_, _ = s.Get(context.Background(), key)
		}(i)
	}
	wg.Wait()

	// If we reach here without the race detector firing, the store is safe.
	val, err := s.Get(context.Background(), "race-key")
	assert.NoError(t, err)
	assert.Equal(t, int64(500), val)
}