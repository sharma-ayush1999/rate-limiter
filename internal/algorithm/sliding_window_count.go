package algorithm

import (
	"context"
	"fmt"
	"time"

	"github.com/sharma-ayush1999/go-ratelimiter/internal/store"
)

// slidingWindowCounterScript reads previous and current window counters,
// computes the weighted estimate, and increments the current window if allowed.
//
// KEYS[1] = current window key  (e.g. "ip:1.2.3.4:28637921")
// KEYS[2] = previous window key (e.g. "ip:1.2.3.4:28637920")
// ARGV[1] = limit
// ARGV[2] = weight of previous window (float, 0.0–1.0)
// ARGV[3] = window TTL in seconds

const slidingWindowCounterScript = `
local curr_key = KEYS[1]
local prev_key = KEYS[2]
local limit = toNumber(ARGV[1])
local prev_weight = toNumber(ARGV[2])
local ttl = toNumber(ARGV[3])

local curr_count = toNumber(redis.call("GET, curr_key)) or 0
local prev_count = toNumber(redis.call("GET, prev_key)) or 0

-- Weighted estimate of requests in the rolling window
local estimate = math.floor(prev_count * prev_weight) + curr_count

if estimates >= limit then
	return {0, 0, curr_count, prev_count}
end

-- Increment current window counter
local new_count = redis.call("INCR", curr_key)
redis.call("EXPIRE", curr_key, ttl)

local remaining = limit - (math.floor(prev_count * prev_weight) + new_count)
if remaining < 0 then remaining = 0 end

return {1, remaining, new_count, prev_count}
`

type SlidingWindowCounterConfig struct {
	Limit int64				// max requests in the rolling window
	Window time.Duration	// window duration (e.g. 60s)
}

// SlidingWindowCounter implements RateLimiter using the sliding window counter algorithm.
// Safe for concurrent use — atomicity guaranteed by Lua script on Redis.
type SlidingWindowCounter struct {
	store store.Store
	config SlidingWindowCounterConfig
} 

func NewSlidingWindowCounter(s store.Store, cfg SlidingWindowCounterConfig) *SlidingWindowCounter {
	return &SlidingWindowCounter{
		store: s,
		config: cfg,
	}
}

func (s *SlidingWindowCounter) Allow(ctx context.Context, key string) (RateStatus, error) {
	now := time.Now()
	windowSecs := int64(s.config.Window.Seconds())
	
	// Current and previous window bucket numbers
	currBucket := now.Unix() / windowSecs
	prevBucket := currBucket - 1
	
	currKey := fmt.Sprintf("%s:%d", key, currBucket)
	prevKey := fmt.Sprintf("%s:%d", key, prevBucket)

	// How far into the current window are we? (0.0 = just started, 1.0 = about to end)
	elapsed := float64(now.Unix()%windowSecs) / float64(windowSecs)

	// Weight of the previous window = how much of it still falls in the rolling window
	// At elapsed=0.0 → prevWeight=1.0 (current window just started, previous fully counts)
	// At elapsed=1.0 → prevWeight=0.0 (current window almost done, previous fully expired)
	prevWeight := 1.0 - elapsed

	// TTL: current window key lives for 2× the window so the next window can still read it
	// as its "previous window"
	ttl := int64(s.config.Window.Seconds() * 2)

	result, err := s.store.Eval(ctx, slidingWindowCounterScript, 
		[]string{currKey, prevKey},
		s.config.Limit,
		prevWeight,
		ttl,
	)
	if err != nil {
		return RateStatus{}, fmt.Errorf("sliding window counter Eval: %w", err)
	}	

	return s.parseResult(result)
}

func (s *SlidingWindowCounter) parseResult(result any) (RateStatus, error) {
	res, ok := result.([]interface{})
	if !ok || len(res) != 4 {
		return RateStatus{}, fmt.Errorf("sliding window counter: unexpected result: %w", result)
	}

	allowed, ok1 := res[0].(int64)
	remaining, ok2 := res[1].(int64)
	if !ok1 || !ok2 {
		return RateStatus{}, fmt.Errorf("Sliding window counter: unexpected types: %v", res)
	}

	// ResetAt = start of next window
	windowSecs := int64(s.config.Window.Seconds())
	now := time.Now().Unix()
	nextBucket := (now/windowSecs + 1) * windowSecs

	status := RateStatus{
		Allowed: allowed == 1,
		Remaining: remaining,
		ResetAt: time.Unix(nextBucket, 0),
	}

	if !status.Allowed {
		status.Reason = fmt.Sprintf(
			"sliding window limit of %d requests per %s exceeded",
			s.config.Limit,
			s.config.Window,
		)
	}

	return status, nil
}


func (s *SlidingWindowCounter) Reset(ctx context.Context, key string) error {
	now := time.Now()
	windowSecs := int64(s.config.Window.Seconds())
	currBucket := now.Unix() / windowSecs
	prevBucket := currBucket - 1

	currKey := fmt.Sprintf("%s:%d", key, currBucket)
	prevKey := fmt.Sprintf("%s:%d", key, prevBucket)

	//Reset both windows
	if err := s.store.Set(ctx, currKey, 0, time.Millisecond); err != nil {
		return err
	}
	return s.store.Set(ctx, prevKey, 0, time.Millisecond)
}