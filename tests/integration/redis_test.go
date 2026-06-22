package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sharma-ayush1999/go-ratelimiter/internal/algorithm"
	"github.com/sharma-ayush1999/go-ratelimiter/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// startRedis spins up a real Redis container for this test.
// Requires Docker to be running.
func startRedis(t *testing.T) *store.RedisStore {
	t.Helper()
	ctx := context.Background()

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image: "redis:7-alpine",
			ExposedPorts: []string{"6379/tcp"},
			WaitingFor: wait.ForListeningPort("6379/tcp"),
		},
		Started: true,
	})
	require.NoError(t, err)

	t.Cleanup(func () { _ = container.Terminate(ctx) })

	host, err := container.Host(ctx)
	require.NoError(t, err)

	port, err := container.MappedPort(ctx, "6379")
	require.NoError(t, err)

	s, err := store.NewRedisStore(store.RedisOptions{
		Addr: fmt.Sprintf("%s:%s", host, port.Port()),
	})

	require.NoError(t, err)

	return s
}

func TestRedis_FixedWindow_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	s := startRedis(t)
	limiter := algorithm.NewFixedWindow(s, algorithm.FixedWindowConfig{
		Limit: 5,
		Window: time.Minute,
	})

	ctx := context.Background()
	for i := 0; i < 5; i++ {
		status, err := limiter.Allow(ctx, "integration-fw")
		require.NoError(t, err)
		assert.True(t, status.Allowed, "request %d should be allowed", i+1)
	}

	status, err := limiter.Allow(ctx, "integration-fw")
	require.NoError(t, err)
	assert.False(t, status.Allowed)
}

func TestRedis_TokenBucket_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	s := startRedis(t)
	limiter := algorithm.NewTokenBucket(s, algorithm.TokenBucketConfig{
		Capacity: 5,
		RefillRate: 1.0,
		Window: time.Minute,
	})

	ctx := context.Background()
	for i := 0; i < 5; i++ {
		status, err := limiter.Allow(ctx, "integration-tb")
		require.NoError(t, err)
		assert.True(t, status.Allowed, "request %d should be allowed", i+1)
	}

	status, err := limiter.Allow(ctx, "integration-tb")
	require.NoError(t, err)
	assert.False(t, status.Allowed)
}

func TestRedis_SlidingWindowLog_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	s := startRedis(t)
	limiter := algorithm.NewSlidingWindowLog(s, algorithm.SlidingWindowLogConfig{
		Limit: 3,
		Window: time.Minute,
	})

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		status, err := limiter.Allow(ctx, "integration-sw1")
		require.NoError(t, err)
		assert.True(t, status.Allowed)
	}

	status, err := limiter.Allow(ctx, "integration-sw1")
	require.NoError(t, err)
	assert.False(t, status.Allowed)
}

func TestRedis_Ping(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	s := startRedis(t)
	err := s.Ping(context.Background())
	assert.NoError(t, err)
}