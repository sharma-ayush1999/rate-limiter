package config

import "time"

// Config is the root config structure — mirrors config.yaml exactly.
type Config struct {
	Server 			ServerConfig	`mapstructure:"server"`
	Redis 			RedisConfig		`mapstructure:"redis"`
	Algorithm 		string			`mapstructure:"algorithm"`	
	FaultTolerance 	FaultConfig		`mapstructure:"fault_tolerance"` // token_bucket | fixed_window | sliding_window_counter | sliding_window_log | leaky_bucket
	Rules 			[]RuleConfig	`mapstructure:"rules"`
}

type ServerConfig struct {
	Port int `mapstructure:"port"`
	ReadTimeout time.Duration `mapstructure:"read_timeout"`
	WriteTimeout time.Duration `mapstructure:"write_timeout"`
}

type RedisConfig struct {
	Addr         string        `mapstructure:"addr"`
	Password     string        `mapstructure:"password"`
	DB           int           `mapstructure:"db"`
	PoolSize     int           `mapstructure:"pool_size"`
	DialTimeout  time.Duration `mapstructure:"dial_timeout"`
	ReadTimeout  time.Duration `mapstructure:"read_timeout"`
	WriteTimeout time.Duration `mapstructure:"write_timeout"`
}

// FaultConfig controls behavior when the store is unavailable.
type FaultConfig struct {
	// OnStoreFailure: "fail_open" (allow all) | "fail_closed" (deny all)
	OnStoreFailure string `mapstructure:"on_store_failure"`
}

// RuleConfig defines a rate limit rule for a specific dimension.
type RuleConfig struct {
	Dimension	string			`mapstructure:"dimension"`	// ip | user | route | global
	Route		string			`mapstructure:"route"`		// only set when dimension=route
	Limit 		int				`mapstructure:"limit"`
	Window		time.Duration	`mapstructure:"window"`

	// Algorithm-specific settings.
	// Only the fields relevant to the configured algorithm are used.
	TokenBucket TokenBucketRule `mapstructure:"token_bucket"`
	LeakyBucket LeakyBucketRule `mapstructure:"leaky_bucket"`
}

// TokenBucketRule holds token bucket specific settings per rule.
// If RefillRate is 0, it defaults to Limit/Window (one full refill per window).
type TokenBucketRule struct {
	Capacity	int64 	`mapstructure:"capacity"`
	RefillRate	float64 `mapstructure:"refill_rate"`
}

// LeakyBucketRule holds leaky bucket specific settings per rule.
// If LeakRate is 0, it defaults to Limit/Window.
type LeakyBucketRule struct {
	Capacity	int64 	`mapstructure:"capacity"`
	LeakRate	float64 `mapstructure:"leak_rate"`
}

