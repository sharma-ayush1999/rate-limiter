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

func TestFixedWindow_AllowsUpToLimit(t *testing.T) {
	s := store.NewMemoryStore()
	limiter := algorithm.NewFixedWindow(s, algorithm.FixedWindowConfig{
		Limit: 5,
		Window: time.Minute,
	})

	for i := 0; i < 5; i++ {
		status, err := limiter.Allow(context.Background(), "test-key")
		require.NoError(t, err)
		assert.True(t, status.Allowed, "request %d should be allowed", i+1)
		assert.Equal(t, int64(4-i), status.Remaining)
	}
}

func TestFixedWindow_DeniesOverLimit(t *testing.T) {
	s := store.NewMemoryStore()
	limiter := algorithm.NewFixedWindow(s, algorithm.FixedWindowConfig{
		Limit: 3,
		Window: time.Minute,
	})

	for i := 0; i < 3; i++ {
		status, err := limiter.Allow(context.Background(), "test-key")
		require.NoError(t, err)
		assert.True(t, status.Allowed)
	}

	// 4th request must be denied
	status, err := limiter.Allow(context.Background(), "test-key")
	require.NoError(t, err)
	assert.False(t, status.Allowed)
	assert.Equal(t, int64(0), status.Remaining)
	assert.NotEmpty(t, status.Reason)

}

func TestFixedWindow_DifferentKeysDontInterfere(t *testing.T) {
	s := store.NewMemoryStore()
	limiter := algorithm.NewFixedWindow(s, algorithm.FixedWindowConfig{
		Limit: 2,
		Window: time.Minute,
	})
	// Exhaust key A
	for i := 0; i < 2; i++ {
		status, err := limiter.Allow(context.Background(), "key-A")
		require.NoError(t, err)
		assert.True(t, status.Allowed)
	}
	statusA, _ := limiter.Allow(context.Background(), "key-A")
	assert.False(t, statusA.Allowed)

	// key B should still be unaffected
	statusB, err := limiter.Allow(context.Background(), "key-B")
	require.NoError(t, err)
	assert.True(t, statusB.Allowed)
}

func TestFixedWindow_ResetAt_isFuture(t *testing.T) {
	s := store.NewMemoryStore()
	limiter := algorithm.NewFixedWindow(s, algorithm.FixedWindowConfig{
		Limit: 10,
		Window: time.Minute,
	})

	status, err := limiter.Allow(context.Background(), "test-key")
	require.NoError(t, err)
	assert.True(t, status.ResetAt.After(time.Now()))
}