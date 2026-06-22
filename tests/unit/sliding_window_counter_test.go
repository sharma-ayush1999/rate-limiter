package unit

import (
	"context"
	"testing"
	"time"

	"github.com/sharma-ayush1999/go-ratelimiter/internal/algorithm"
	"github.com/sharma-ayush1999/go-ratelimiter/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSlidingWindowCOunter_AllowsUpToLimit(t *testing.T){
	s := store.NewMemoryStore()
	limiter := algorithm.NewSlidingWindowCounter(s, algorithm.SlidingWindowCounterConfig{
		Limit: 5,
		Window: time.Minute,
	})

	for i := 0; i < 5; i++ {
		status, err := limiter.Allow(context.Background(), "swc-key")
		require.NoError(t, err)
		assert.True(t, status.Allowed, "request %d should be allowed", i+1)
	}
}

func TestSlidingWindowCounter_DeniesOverLimit(t *testing.T) {
	s := store.NewMemoryStore()
	limiter := algorithm.NewSlidingWindowCounter(s, algorithm.SlidingWindowCounterConfig{
		Limit: 3,
		Window: time.Minute,
	})

	for i := 0; i < 3; i++ {
		_, _ = limiter.Allow(context.Background(), "swc-key")
	}

	status, err := limiter.Allow(context.Background(), "swc-key")
	require.NoError(t, err)
	assert.False(t, status.Allowed)
	assert.NotEmpty(t, status.Reason)
}

func TestSlidingWindowCounter_DifferentKeysDontInterfere(t *testing.T) {
	s := store.NewMemoryStore()
	limiter := algorithm.NewSlidingWindowCounter(s, algorithm.SlidingWindowCounterConfig{
		Limit: 2,
		Window: time.Minute,
	})

	_, _ = limiter.Allow(context.Background(), "swc-A")
	_, _ = limiter.Allow(context.Background(), "swc-A")
	statusA, _ := limiter.Allow(context.Background(), "swc-A")
	assert.False(t, statusA.Allowed)
	
	statusB, err := limiter.Allow(context.Background(), "swc-B")
	require.NoError(t, err)
	assert.True(t, statusB.Allowed)
}