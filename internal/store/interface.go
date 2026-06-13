package store

import (
	"context"
	"time"
)

// Store is the abstraction over Redis (or any backend).
// All implementations MUST be safe for concurrent use by multiple goroutines.
type Store interface {
	// Get returns the value at key. Returns 0 if key does not exist.
	Get(ctx context.Context, key string) (int64, error)

	// Set sets key to value with a TTL. Overwrites existing value.
	Set(ctx context.Context, key string, value int64, ttl time.Duration) error

	// Increment atomically increments key by 1 and sets TTL if key is new.
	// Returns the value after increment.
	Increment(ctx context.Context, key string, ttl time.Duration) (int64, error)

	// Eval executes a Lua script atomically on the store.
	// keys and args map to KEYS and ARGV in the Lua script.
	// Returns the raw result from the script.
	Eval(ctx context.Context, script string, keys []string, args ...any) (any, error)

	// Ping checks if the store is reachable.
	Ping(ctx context.Context) error


}