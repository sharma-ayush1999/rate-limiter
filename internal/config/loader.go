package config

import (
	"fmt"
	"strings"

	"github.com/spf13/viper"
)

// Load reads config.yaml from the given path and applies any
// environment variable overrides (e.g. RL_REDIS_ADDR overrides redis.addr).
func Load(path string) (*Config, error) {
	v := viper.New()

	//File config
	v.SetConfigFile(path)
	v.SetConfigType("yaml")


	// Environment variable overrides.
	// RL_ prefix + underscores replace dots: RL_REDIS_ADDR → redis.addr
	v.SetEnvPrefix("RL")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	
	// Defaults — used when the key is absent from config.yaml and env
	v.SetDefault("server.port", 8080)
	v.SetDefault("server.read_timeout", "5s")
	v.SetDefault("server.write_timeout", "5s")
	v.SetDefault("redis.addr", "localhost:6379")
	v.SetDefault("redis.db", 0)
	v.SetDefault("redis.pool_size", 10)
	v.SetDefault("algorithm", "token_bucket")
	v.SetDefault("fault_tolerance.on_store_failure", "fail_open")

	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("reading config file %q: %w", path, err)
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	if err := validate(&cfg); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return &cfg, nil
}


// validate catches obvious misconfigurations at startup rather than at runtime.
func validate(cfg *Config) error {
	validAlgos := map[string]bool {
		"token_bucket": true,
		"leaky_bucket": true,
		"sliding_window_counter": true,
		"sliding_window_log": true,
		"fixed_window": true,
	}

	if !validAlgos[cfg.Algorithm] {
		return fmt.Errorf("unknown algorithm %q - must be one of token_bucket, fixed_window, sliding_window_counter, sliding_window_log, leaky_bucket", cfg.Algorithm)
	}

	validFailModes := map[string]bool {
		"fail_open": true,
		"fail_closed": true,
	}

	if !validFailModes[cfg.FaultTolerance.OnStoreFailure] {
		return fmt.Errorf("unknown on_store_failure %q - must be fail_open or fail_closed", cfg.FaultTolerance.OnStoreFailure)
	}

	validDimensions := map[string]bool {
		"ip": true,
		"user": true,
		"route": true,
		"global": true,
	}

	for i, rule := range cfg.Rules {
		if !validDimensions[rule.Dimension] {
			return fmt.Errorf("rule[%d]: unknown dimension %q", i, rule.Dimension)
		}
		if rule.Dimension == "route" && rule.Route == "" {
			return fmt.Errorf("route[%d]: dimension=route requires a route field", i)
		}
		if rule.Limit <= 0 {
			return fmt.Errorf("rule[%d]: limit must be > 0", i)
		}
		if rule.Window <= 0 {
			return fmt.Errorf("rule[%d]: window must be > 0", i)
		}
	}
	return nil
}