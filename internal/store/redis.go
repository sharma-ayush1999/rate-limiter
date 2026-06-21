package store

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)




type RedisStore struct {
	client *redis.Client
}

// RedisOptions holds all Redis connection settings.
// Mirrors config.RedisConfig but lives in the store package
// to keep the store independent of the config package.
type RedisOptions struct {
	Addr         string
	Password     string
	DB           int
	PoolSize     int
	DialTimeout  time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
}

func NewRedisStore(opts RedisOptions) (*RedisStore, error) {
	// Apply sensible defaults for anything not set
	if opts.PoolSize == 0 {
		opts.PoolSize = 10
	}
	if opts.DialTimeout == 0 {
		opts.DialTimeout = 5 * time.Second
	}
	if opts.ReadTimeout == 0 {
		opts.ReadTimeout = 3 * time.Second
	}
	if opts.WriteTimeout == 0 {
		opts.WriteTimeout = 3 * time.Second
	}

	client := redis.NewClient(&redis.Options{
		Addr:         opts.Addr,
		Password:     opts.Password,
		DB:           opts.DB,
		PoolSize:     opts.PoolSize,
		DialTimeout:  opts.DialTimeout,
		ReadTimeout:  opts.ReadTimeout,
		WriteTimeout: opts.WriteTimeout,
	})

	// Verify connection on startup — fail fast rather than discovering
	// Redis is unreachable on the first real request.
	if err := client.Ping(context.Background()).Err(); err != nil {
		return nil, fmt.Errorf("redis connect failed: %w", err)
	}

	return &RedisStore{client: client}, nil
}

func (r *RedisStore) Get(ctx context.Context, key string) (int64, error){
	val, err := r.client.Get(ctx, key).Int64()
	if err == redis.Nil{
		return 0, nil 	// key doesn't exist — not an error
	}
	return val, err
} 

func (r *RedisStore) Set(ctx context.Context, key string, value int64, ttl time.Duration) error {
	return r.client.Set(ctx, key, value, ttl).Err()
}

func (r *RedisStore) Increment(ctx context.Context, key string, ttl time.Duration) (int64, error) {
	pipe := r.client.TxPipeline()
	incr := pipe.Incr(ctx, key)
	pipe.Expire(ctx, key, ttl)

	_, err := pipe.Exec(ctx)
	if err != nil {
		return 0, err
	}
	return incr.Val(), nil
}

func (r *RedisStore) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error){
	return r.client.Eval(ctx, script, keys, args...).Result()
}

func (r *RedisStore) Ping(ctx context.Context) error {
	return r.client.Ping(ctx).Err()
}