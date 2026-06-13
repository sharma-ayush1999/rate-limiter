package algorithm

import (
	"context"
	"time"
)

// RateStatus holds the result of a rate limit check.
type RateStatus struct {
	Allowed		bool
	Remaining 	int64		// requests left in current window
	ResetAt 	time.Time	// when the window/bucket resets
	Reason 		string		// populated only when Allowed == false
}

// RateLimiter is the strategy interface every algorithm must implement.
// All implementations MUST be safe for concurrent use by multiple goroutines.
type RateLimiter interface {
	// Allow checks whether the request identified by key is within quota.
	// key is a pre-built string like "ip:1.2.3.4" or "user:abc123".
	Allow(ctx context.Context, key string) (RateStatus, error)

	// Reset clears the rate limit state for the given key.
	Reset(ctx context.Context, key string) error

}