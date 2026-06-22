package algorithm

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/sharma-ayush1999/go-ratelimiter/internal/store"
)

// tokensKey / refillKey suffixes for the native in-memory path.
const (
	nativeTokSuffix    = ":mem:t"
	nativeRefillSuffix = ":mem:r"
)

// tokenBucketScript is the Lua script executed atomically on Redis.
// Defined at package level so it's compiled once, not on every request.
const tokenBucketScript = `
local key = KEYS[1]
local capacity = tonumber(ARGV[1])
local rate = tonumber(ARGV[2])
local now = tonumber(ARGV[3])
local ttl = tonumber(ARGV[4])

local data = redis.call("HMGET", key, "tokens", "last_refill")
local tokens = tonumber(data[1])
local last_refill = tonumber(data[2])

if tokens == nil then 
	tokens = capacity
	last_refill = now
end

local elapsed = math.max(0, now - last_refill)
local refill = math.floor(elapsed * rate)
tokens = math.min(capacity, tokens + refill)

if refill > 0 then 
	last_refill = now
end

local allowed = 0
if tokens >= 1 then
	tokens = tokens - 1
	allowed = 1
end

redis.call("HMSET", key, "tokens", tokens, "last_refill", last_refill)
redis.call("EXPIRE", key, ttl)

return {allowed, tokens}
`

// TokenBucketConfig holds the settings for this algorithm.
type TokenBucketConfig struct {
	Capacity int64			// max tokens the bucket can hold
	RefillRate float64		// tokens added per second
	Window time.Duration	// TTL for the Redis key (should be > capacity/refill_rate)
}

// TokenBucket implements the RateLimiter interface using the token bucket algorithm.
// Safe for concurrent use by multiple goroutines.
type TokenBucket struct {
	store     store.Store
	config    TokenBucketConfig
	mu        sync.Mutex // guards native in-memory path
	useNative bool       // true when store is MemoryStore (no Lua support)
}

// NewTokenBucket creates a new TokenBucket limiter.
func NewTokenBucket(s store.Store, cfg TokenBucketConfig) *TokenBucket {
	_, isMemory := s.(*store.MemoryStore)
	return &TokenBucket{store: s, config: cfg, useNative: isMemory}
}

func (t *TokenBucket) Allow(ctx context.Context, key string) (RateStatus, error) {
	if t.useNative {
		return t.allowNative(ctx, key)
	}

	now := float64(time.Now().UnixNano()) / 1e9
	ttl := int64(t.config.Window.Seconds())

	result, err := t.store.Eval(ctx, tokenBucketScript,
		[]string{key},
		t.config.Capacity,
		t.config.RefillRate,
		now,
		ttl,
	)
	if err != nil {
		return RateStatus{}, fmt.Errorf("token bucket eval: %w", err)
	}
	return t.parseResult(result)
}

// allowNative is the MemoryStore path — implements token bucket using
// Get/Set + mutex instead of a Lua script.
func (t *TokenBucket) allowNative(ctx context.Context, key string) (RateStatus, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	tokKey    := key + nativeTokSuffix
	refillKey := key + nativeRefillSuffix

	tokensVal, _    := t.store.Get(ctx, tokKey)
	lastRefillVal, _ := t.store.Get(ctx, refillKey)

	now := time.Now().Unix()
	var tokens int64
	lastRefill := lastRefillVal

	if lastRefill == 0 {
		// First request — start with a full bucket.
		tokens = t.config.Capacity
		lastRefill = now
	} else {
		tokens = tokensVal
		elapsed := now - lastRefill
		refill  := int64(float64(elapsed) * t.config.RefillRate)
		if refill > 0 {
			tokens += refill
			if tokens > t.config.Capacity {
				tokens = t.config.Capacity
			}
			lastRefill = now
		}
	}

	allowed := tokens >= 1
	if allowed {
		tokens--
	}

	_ = t.store.Set(ctx, tokKey,    tokens,     t.config.Window)
	_ = t.store.Set(ctx, refillKey, lastRefill, t.config.Window)

	status := RateStatus{
		Allowed:   allowed,
		Remaining: tokens,
		ResetAt:   time.Now().Add(time.Duration(float64(time.Second) / t.config.RefillRate)),
	}
	if !allowed {
		status.Reason = fmt.Sprintf("rate limit exceeded, %d tokens remaining", tokens)
	}
	return status, nil
}

// parseResult converts the raw Lua return value into a RateStatus.
// Redis returns Lua arrays as []interface{} where each element is int64.
func (t *TokenBucket) parseResult(result any) (RateStatus, error) {
	// Redis returns Lua {allowed, tokens} as []interface{}{int64, int64}
	res, ok := result.([]interface{})
	if !ok || len(res) != 2 {
		return RateStatus{}, fmt.Errorf("token bucket: unexpected result format: %v", result)
	}

	allowed, ok1 := res[0].(int64)
	remaining, ok2 := res[1].(int64)

	if !ok1 ||!ok2 {
		return RateStatus{}, fmt.Errorf("token bucket: unexpected result types: %v", res)
	}

	status := RateStatus{
		Allowed: allowed == 1,
		Remaining: remaining,
		// ResetAt: approximate — next full token will arrive in 1/RefillRate seconds
		ResetAt: time.Now().Add(time.Duration(float64(time.Second) / t.config.RefillRate)),
	}

	if !status.Allowed {
		status.Reason = fmt.Sprintf("rate limit exceeded, %d tokens remaining", remaining)
	}

	return status, nil
}

func (t *TokenBucket) Reset(ctx context.Context, key string) error {
	if t.useNative {
		_ = t.store.Set(ctx, key+nativeTokSuffix,    0, time.Millisecond)
		_ = t.store.Set(ctx, key+nativeRefillSuffix, 0, time.Millisecond)
		return nil
	}
	return t.store.Set(ctx, key, 0, time.Millisecond)
}