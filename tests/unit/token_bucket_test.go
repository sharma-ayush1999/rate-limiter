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

func TokenBucketTest_AllowsUpToCapacity(t *testing.T) {
	s := store.NewMemoryStore()
	limiter := algorithm.NewTokenBucket(s, algorithm.TokenBucketConfig{
		Capacity: 5,
		RefillRate: 1.0,
		Window: time.Minute,
	})

	for i := 0; i < 5; i++ {
		status, err := limiter.Allow(context.Background(), "tb-key")
		require.NoError(t, err)
		assert.True(t, status.Allowed, "request %d should be allowed", i+1)
	}

}

func TestTokenBucket_DeniesWhenEmpty(t *testing.T) {
	s := store.NewMemoryStore()
	limiter := algorithm.NewTokenBucket(s, algorithm.TokenBucketConfig{
		Capacity: 3,
		RefillRate: 1.0,
		Window: time.Minute,
	})

	for i := 0; i < 3; i++ {
		_, _ = limiter.Allow(context.Background(), "tb-key")
	}

	status, err := limiter.Allow(context.Background(), "tb-key")
	require.NoError(t, err)
	assert.False(t, status.Allowed)
	assert.Equal(t, int64(0), status.Remaining)
}


func TestTokenBucket_Reset_Clear_State(t *testing.T) {
	s := store.NewMemoryStore()
	limiter := algorithm.NewTokenBucket(s, algorithm.TokenBucketConfig{
		Capacity: 2,
		RefillRate: 1.0,
		Window: time.Minute,
	})

	_, _ = limiter.Allow(context.Background(), "tb-key")
	_, _ = limiter.Allow(context.Background(), "tb-key")

	// Should be denied now
	status, _ := limiter.Allow(context.Background(), "tb-key")
	assert.False(t, status.Allowed)

	// Reset and try again
	err := limiter.Reset(context.Background(), "tb-key")
	require.NoError(t, err)

	status, err = limiter.Allow(context.Background(), "tb-key")
	require.NoError(t, err)
	assert.True(t,status.Allowed)

}