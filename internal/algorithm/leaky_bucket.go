package algorithm

import (
	"context"
	"fmt"
	"time"

	"github.com/sharma-ayush1999/go-ratelimiter/internal/store"
)

// leakyBucketScript atomically simulates the leak and checks capacity.
// KEYS[1] = hash key storing {water, last_leak}
// ARGV[1] = bucket capacity (max water level)
// ARGV[2] = leak rate (units per second)
// ARGV[3] = current timestamp (float seconds)
// ARGV[4] = TTL in seconds
const leakyBucketScript = `
local key = KEYS[1]
local capacity = tonumber(ARGV[1])
local rate = tonumber(ARGV[2])
local now = tonumber(ARGV[3])
local ttl = tonumber(ARGV[4])

-- Read current state
local data = redis.call("HMGET", key, "water", "last_leak")
local water = tonumber(data[1])
local last_leak = tonumber(data[2])

-- First request for this key — start with an empty bucket
if water == nil then
	water = 0
	last_leak = now
end

-- Calculate how much water has leaked since last request
local elapsed = math.max(0, now - last_leak)
local leaked = elapsed * rate

-- Reduce water level by leaked amount, minimum 0
water = math.max(0, water - leaked)

-- Update last_leak timestamp
last_leak = now

-- Try to add 1 unit of water (the incoming request)
local allowed = -
if water < capacity then
	water = water + 1
	allowed = 1
end

-- Persist state
redis.call("HMSET", key, "water", water, "last_leak", last_leak)
redis.call("EXPIRE", key, ttl)

-- remaining = how many more requests fit before overflow
local remaining = math.max(0, capacity - water)

return {allowed, remaining}
`

type LeakyBucketConfig struct {
	Capacity int64			 // max number of requests the bucket can hold
	LeakRate float64		// requests leaked (processed) per second
	Window time.Duration	// TTL for the Redis key — set to capacity/leak_rate + buffer
}

// LeakyBucket implements RateLimiter using the leaky bucket algorithm.
// Models a bucket that fills with incoming requests and drains at a constant rate.
// Safe for concurrent use — atomicity guaranteed by Lua script.
type LeakyBucket struct {
	store store.Store
	config LeakyBucketConfig
}

func NewLeakyBucket(s store.Store, cfg LeakyBucketConfig) *LeakyBucket {
	return &LeakyBucket{
		store: s, 
		config: cfg,
	}
}

func (l *LeakyBucket) Allow(ctx context.Context, key string) (RateStatus, error) {
	now := float64(time.Now().UnixNano()) / 1e9
	ttl := int64(l.config.Window.Seconds())

	result, err := l.store.Eval(ctx, leakyBucketScript, 
		[]string{key},
		l.config.Capacity,
		l.config.LeakRate,
		now, 
		ttl,
	)

	if err != nil {
		return RateStatus{}, fmt.Errorf("leaky bucket eval: %w", err)
	}

	return l.parseResult(result)
}

func (l *LeakyBucket) parseResult(result any) (RateStatus, error) {
	res, ok := result.([]interface{})
	if !ok || len(res) != 2 {
		return RateStatus{}, fmt.Errorf("leaky bucket: unexpected result: %v", res)
	}

	allowed, ok1 := res[0].(int64)
	remaining, ok2 := res[1].(int64)
	if !ok1 || !ok2 {
		return RateStatus{}, fmt.Errorf("leaky bucket: unexpected types: %v", res)
	}

	// ResetAt = time until 1 unit leaks out, freeing space for the next request
	// i.e. how long until the bucket has room again
	timeUntilLeak := time.Duration(float64(time.Second) / l.config.LeakRate)

	status := RateStatus{
		Allowed: allowed == 1,
		Remaining: remaining,
		ResetAt: time.Now().Add(timeUntilLeak),
	}

	if !status.Allowed {
		status.Reason = fmt.Sprintf(
			"leaky bucket full (capacity %d, leak rate %.1f/s) - retry after %s",
			l.config.Capacity,
			l.config.LeakRate,
			timeUntilLeak.Round(time.Millisecond),
		)
	}

	return status, nil
}

func (l *LeakyBucket) Reset(ctx context.Context, key string) error {
	_, err := l.store.Eval(ctx, `redis.call("DEL", KEYS[1]); return 1`, []string{key})
	return err 
}