package algorithm

import (
	"context"
	"fmt"
	"time"

	"github.com/sharma-ayush1999/go-ratelimiter/internal/store"
)

// FixedWindowConfig holds settings for the fixed window algorithm.
type FixedWindowConfig struct {
	Limit int64				// max requests allowed per window
	Window time.Duration	// duration of each window (e.g. 60s, 1h)
}


// FixedWindow implements RateLimiter using the fixed window algorithm.
// Safe for concurrent use — atomicity is guaranteed by Redis INCR + EXPIRE pipeline.
type FixedWindow struct {
	store store.Store
	config FixedWindowConfig
}

func NewFixedWindow(s store.Store, cfg FixedWindowConfig) *FixedWindow {
	return &FixedWindow{
		store: s,
		config: cfg,
	}
}

func (f *FixedWindow) Allow(ctx context.Context, key string) (RateStatus, error) {
	// windowKey ties the counter to the current time window.
	// e.g. key="ip:1.2.3.4", window=60s → windowKey="ip:1.2.3.4:1718275200"
	// When the window rolls over, the timestamp changes → new key → fresh counter.
	windowKey := f.windowKey(key)

	count, err := f.store.Increment(ctx, windowKey, f.config.Window)
	if err != nil {
		return RateStatus{}, fmt.Errorf("fixed window increment: %w", err)
	}
	
	remaining := f.config.Limit - count
	if remaining < 0 {
		remaining = 0
	}

	allowed := count <= f.config.Limit

	status := RateStatus{
		Allowed: allowed,
		Remaining: remaining,
		ResetAt: f.nextWindowStart(),
	}

	if !allowed {
		status.Reason = fmt.Sprintf(
			"limit of %d requests per %s exceeded",
			f.config.Limit,
			f.config.Window,
		)
	}

	return status, nil
}


// windowKey builds a Redis key scoped to the current time window.
// Dividing UnixSeconds by window seconds gives a bucket number that
// increments by 1 each time the window rolls over.
func (f *FixedWindow) windowKey(key string) string {
	windowSeconds := int64(f.config.Window.Seconds())
	bucket := time.Now().Unix() / windowSeconds
	return fmt.Sprintf("%s:%d", key, bucket)
}


// nextWindowStart returns the exact time the current window ends
// and the next one begins — used to populate ResetAt in the response.
func (f *FixedWindow) nextWindowStart() time.Time {
	windowSeconds := int64(f.config.Window.Seconds())
	now := time.Now().Unix()

	// Round up to the next window boundary
	nextBucket := (now/windowSeconds + 1) * windowSeconds
	return time.Unix(nextBucket, 0)
}


func (f *FixedWindow) Reset(ctx context.Context, key string) error {
	// Reset the current window's key. Past windows expired on their own via TTL.
	return f.store.Set(ctx, f.windowKey(key), 0, time.Millisecond)
}