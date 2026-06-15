package algorithm

import (
	"context"
	"fmt"
	"time"

	"github.com/sharma-ayush1999/go-ratelimiter/internal/store"
)

// slidingWindowLogScript atomically:
//  1. Removes all timestamps outside the rolling window
//  2. Counts remaining timestamps
//  3. Adds the current timestamp only if under limit
//
// KEYS[1] = sorted set key for this rate limit key
// ARGV[1] = current timestamp in microseconds (score)
// ARGV[2] = window start timestamp in microseconds (now - window)
// ARGV[3] = limit (max requests)
// ARGV[4] = TTL in seconds
const slidingWindowLogScript = `
local key = KEYS[1]
local now = tonumber(ARGV[1])
local window_start = tonumber(ARGV[2])
local limit = tonumber(ARGV[3])
local ttl = tonumber(ARGV[4])

-- Step 1: Remove timestamps that have fallen outside the window
redis.call("ZREMRANGEBYSCORE", key, "-inf", window_start)

-- Step 2: Count how many timestamps remain (= requests in current window)
local count = redis.call("ZCARD", key)

-- Step 3: If under limit, record this request's timestamp and allow it
if count < limit then
	-- Score = timestamp, member = timestamp (unique per microsecond)
    -- Using microseconds avoids collisions from requests arriving in the same millisecond
    redis.call("ZADD", key, now, now)
	redis.call("EXPIRE", key, ttl)
	local remaining = limit - count - 1
	return {1, remaining}
end

-- Over limit — do not record, deny
local remaining = 0
return {0, remaining}
`

type SlidingWindowLogConfig struct {
	Limit int64				 // max requests allowed in the rolling window
	Window time.Duration	// rolling window duration (e.g. 60s)
}

// SlidingWindowLog implements RateLimiter using exact timestamp logging.
// Each request's timestamp is stored in a Redis sorted set.
// Safe for concurrent use — atomicity guaranteed by Lua script.
type SlidingWindowLog struct {
	store store.Store
	config SlidingWindowLogConfig
}

func NewSlidingWindowLog(s store.Store, cfg SlidingWindowLogConfig) *SlidingWindowLog {
	return &SlidingWindowLog{
		store: s,
		config: cfg,
	}
}

func (s * SlidingWindowLog) Allow(ctx context.Context, key string) (RateStatus, error) {
	now := time.Now()

	// Use microseconds for score precision.
	// Microseconds ensure uniqueness even under very high concurrency
	// (two requests in the same nanosecond get different scores).
	nowMicro := now.UnixMicro()

	// window_start = earliest timestamp still inside the rolling window
	windowStart := now.Add(-s.config.Window).UnixMicro()

	// TTL: keep the sorted set alive for one full window after the last request
	ttl := int64(s.config.Window.Seconds()) + 1

	result, err := s.store.Eval(ctx, slidingWindowLogScript, 
		[]string{key},
		nowMicro,
		windowStart,
		s.config.Limit,
		ttl,
	)

	if err != nil {
		return RateStatus{}, fmt.Errorf("sliding window log eval: %w", err)
	}

	return s.parseResult(now, result)
}

func (s *SlidingWindowLog) parseResult(now time.Time, result any) (RateStatus, error) {
	res, ok := result.([]interface{})
	if !ok || len(res) != 2{
		return RateStatus{}, fmt.Errorf("sliding indow log: unexpected result: %v", res)
	}

	allowed, ok1 := res[0].(int64)
	remaining, ok2 := res[1].(int64)
	if !ok1 || !ok2 {
		return RateStatus{}, fmt.Errorf("sliding indow log: unexpected result: %v", res)
	}

	status := RateStatus{
		Allowed: allowed == 1,
		Remaining: remaining,
		// ResetAt = when the oldest request in the window falls out
		// Approximated as now + window — exact value would require another Redis call
		ResetAt: now.Add(s.config.Window),
	}

	if !status.Allowed {
		status.Reason = fmt.Sprintf(
			"sliding window log limit pf %d requests [er %s exceeded",
			s.config.Limit,
			s.config.Window,
		)
	}

	return status, nil
}

func (s *SlidingWindowLog) Reset(ctx context.Context, key string) error{
	// Delete all members from the sorted set by removing scores from -inf to +inf
	_, err := s.store.Eval(ctx, `redis.call("DEL", KEYS[1]); return 1`, []string{key})
	return err
}