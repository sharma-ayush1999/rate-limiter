package store

import (
	"context"
	"fmt"
	"time"

	"github.com/sony/gobreaker"
)

// FailPolicy controls what happens when the circuit breaker is open.
type FailPolicy int

const (
	// FailOpen allows all requests when the store is unavailable.
	// Safer for user experience — traffic gets through even if limiting is broken.
	FailOpen FailPolicy = iota

	// FailClosed denies all requests when the store is unavailable.
	// Safer for protecting downstream services — no traffic passes during outage.
	FailClosed
)

// CircuitBreakerStore wraps any Store with a circuit breaker.
// All store calls go through the breaker — when it trips open,
// calls immediately return the configured fail policy result.
type CircuitBreakerStore struct {
	inner 	Store
	cb		*gobreaker.CircuitBreaker
	policy	FailPolicy
}

func NewCircuitBreakerStore(inner Store, policy FailPolicy) *CircuitBreakerStore {
	settings := gobreaker.Settings{
		// Trip open after 5 consecutive failures
		MaxRequests: 1,					 // allow 1 request in half-open to probe recovery
		Interval: 60 * time.Second,		// reset failure count after 60s of normal operation
		Timeout: 10 * time.Second,		// stay open for 10s before moving to half-open

		// ReadyToTrip is called after each failure.
		// We trip open after 5 consecutive failures.
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			return counts.ConsecutiveFailures >= 5
		},
	}
	return &CircuitBreakerStore{
		inner:	inner,
		cb:		gobreaker.NewCircuitBreaker(settings),
		policy: policy,
	}
}

// call executes fn through the circuit breaker.
// If the breaker is open it returns an ErrOpenState immediately — no Redis call made.
func (c *CircuitBreakerStore) call(fn func() (any, error)) (any, error) {
	return c.cb.Execute(fn)
}

// isOpen returns true when the breaker has tripped open.
func (c *CircuitBreakerStore) isOpen(err error) bool {
	return err == gobreaker.ErrOpenState || err == gobreaker.ErrTooManyRequests
}

func (c *CircuitBreakerStore) Get(ctx context.Context, key string) (int64, error) {
	result, err := c.call(func() (any, error) {
		return c.inner.Get(ctx, key)
	})

	if err != nil {
		if c.isOpen(err) {
			return c.failInt(), nil
		}
	}
	return result.(int64), nil
}

func (c *CircuitBreakerStore) Set(ctx context.Context, key string, value int64, ttl time.Duration) error {
	_, err := c.call(func() (any, error) {
		return nil, c.inner.Set(ctx, key, value, ttl)
	})

	if err != nil {
		if c.isOpen(err) {
			return nil	// swallow — a failed Set is non-critical during outage
		}
		return err
	}
	return nil
}

func (c *CircuitBreakerStore) Increment(ctx context.Context, key string, ttl time.Duration) (int64, error) {
	result, err := c.call(func () (any, error) {
		return c.inner.Increment(ctx, key, ttl)
	})

	if err != nil {
		if c.isOpen(err) {
			return c.failInt(), nil
		}
		return 0, err
	}
	return result.(int64), nil
}

func (c *CircuitBreakerStore) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	result, err := c.call(func() (any, error) {
		return c.inner.Eval(ctx, script, keys, args...)
	})

	if err != nil {
		if c.isOpen(err) {
			return c.failEval(), nil
		}
		return nil, err
	}
	return result, nil
}

func (c *CircuitBreakerStore) Ping(ctx context.Context) error {
	_, err := c.call(func () (any, error) {
		return nil, c.inner.Ping(ctx)
	})

	if err != nil {
		if c.isOpen(err){
			return fmt.Errorf(("circuit breaker open: redis unavailable"))
		}
		return err
	}
	return nil
}

// failInt returns a value that causes the algorithm to allow or deny
// based on the configured policy.
//
// For algorithms using Get/Increment:
//   - FailOpen  → return 0 (looks like no requests made → allow)
//   - FailClosed → return math.MaxInt64 (looks like limit exceeded → deny)
func (c *CircuitBreakerStore) failInt() int64 {
	if c.policy == FailOpen {
		return 0
	}
	return int64(^uint(0) >> 1) // math.MaxInt64
}

// failEval returns a Lua result that causes the algorithm to allow or deny.
// All our Lua scripts return {allowed, remaining} where allowed=1 means allow.
func (c *CircuitBreakerStore) failEval() any {
	if (c.policy == FailOpen) {
		// Mimic a successful allow: {allowed=1, remaining=1}
		return []interface{}{int64(1), int64(1)}
	}
	// Mimic a denial: {allowed=0, remaining=0}
	return []interface{}{int64(0), int64(0)}
}