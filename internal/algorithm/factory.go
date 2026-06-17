package algorithm

import (
	"fmt"
	"time"

	"github.com/sharma-ayush1999/go-ratelimiter/internal/config"
	"github.com/sharma-ayush1999/go-ratelimiter/internal/store"
)

// New reads the algorithm name and rule from config and returns
// a ready-to-use RateLimiter backed by the given store.
// This is the only place in the codebase that knows which concrete
// algorithm type maps to which config string.
func New(algoName string, rule config.RuleConfig, s store.Store) (RateLimiter, error) {
	switch algoName {
	case "token_bucket":
		return newTokenBucket(rule, s), nil
	case "fixed_window":
		return newFixedWindow(rule, s), nil
	case "sliding_window_counter":
		return newSlidingWindowCounter(rule, s), nil
	case "sliding_window_log":
		return newSlidingWindowLog(rule, s), nil
	case "leaky_bucket":
		return newLeakyBucket(rule, s), nil
	default:
		// validate() in config/loader.go catches this before we ever get here.
		// This default is a safety net.
		return nil, fmt.Errorf("unknown algorithm: %q", algoName)
	}
}

// --- private constructors — translate RuleConfig into algo-specific configs ---
func newTokenBucket(rule config.RuleConfig, s store.Store) RateLimiter {
	capacity := rule.TokenBucket.Capacity

	if capacity == 0 {
		capacity = int64(rule.Limit) // default: capacity = limit
	}

	refillRate := rule.TokenBucket.RefillRate
	if refillRate == 0 {
		// Default: refill the full bucket over one window
		// e.g. limit=60, window=60s → refillRate=1 token/sec
		refillRate = float64(rule.Limit) / rule.Window.Seconds()
	}

	return NewTokenBucket(s, TokenBucketConfig{
		Capacity: capacity,
		RefillRate: refillRate,
		Window: rule.Window,
	})
}

func newFixedWindow(rule config.RuleConfig, s store.Store) RateLimiter {
	return NewFixedWindow(s, FixedWindowConfig{
		Limit: int64(rule.Limit),
		Window: rule.Window,
	})
}

func newSlidingWindowCounter(rule config.RuleConfig, s store.Store) RateLimiter {
	return NewSlidingWindowCounter(s, SlidingWindowCounterConfig{
		Limit: int64(rule.Limit),
		Window: rule.Window,
	})
}

func newSlidingWindowLog(rule config.RuleConfig, s store.Store) RateLimiter {
	return NewSlidingWindowLog(s, SlidingWindowLogConfig{
		Limit: int64(rule.Limit),
		Window: rule.Window,
	})
}

func newLeakyBucket(rule config.RuleConfig, s store.Store) RateLimiter {
	capacity := rule.LeakyBucket.Capacity
	if capacity == 0 {
		capacity = int64(rule.Limit)
	}

	leakRate := rule.LeakyBucket.LeakRate
	if leakRate == 0 {
		leakRate = float64(rule.Limit) / rule.Window.Seconds()
	}

	// TTL = time to drain full bucket × 2 buffer
	ttl := time.Duration(float64(capacity)/leakRate*2) *time.Second

	return NewLeakyBucket(s, LeakyBucketConfig{
		Capacity: capacity,
		LeakRate: leakRate,
		Window: ttl,
	})
}