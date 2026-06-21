package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/sharma-ayush1999/go-ratelimiter/internal/algorithm"
	"github.com/sharma-ayush1999/go-ratelimiter/internal/config"
	"github.com/sharma-ayush1999/go-ratelimiter/internal/handler"
	"github.com/sharma-ayush1999/go-ratelimiter/internal/store"
	"go.uber.org/zap"
)


func main() {
	// --- Logger ---
	log, _ := zap.NewProduction()
	defer log.Sync()

	// --- Config ---
	cfg, err := config.Load("config.yaml")
	if err != nil {
		log.Fatal("failed to load config", zap.Error(err))
	}
		
	// --- Store ---
	redisStore, err := store.NewRedisStore(store.RedisOptions{
		Addr:         cfg.Redis.Addr,
		Password:     cfg.Redis.Password,
		DB:           cfg.Redis.DB,
		PoolSize:     cfg.Redis.PoolSize,
		DialTimeout:  cfg.Redis.DialTimeout,
		ReadTimeout:  cfg.Redis.ReadTimeout,
		WriteTimeout: cfg.Redis.WriteTimeout,
	})
	if err != nil {
		log.Fatal("failed to connect to redis", zap.Error(err))
	}
	log.Info("connected to redis",
		zap.String("addr", cfg.Redis.Addr),
		zap.Int("pool_size", cfg.Redis.PoolSize),
	)

	// Wrap with circuit breaker — trips open after 5 consecutive Redis failures.
	// Falls back to the configured fail policy (fail_open or fail_closed).
	policy := store.FailOpen
	if cfg.FaultTolerance.OnStoreFailure == "fail_closed" {
		policy = store.FailClosed
	}
	cbStore := store.NewCircuitBreakerStore(redisStore, policy)

	// --- Build one limiter per rule ---
	// Key format matches resolveRule() in handler/check.go:
	//   "ip", "user", "global", "route:/api/v1/login"
	limiters := make(map[string]algorithm.RateLimiter)

	for _, rule := range cfg.Rules {
		limiter, err := algorithm.New(cfg.Algorithm, rule, cbStore)
		if err != nil {
			log.Fatal("failed to create limiter",
			zap.String("dimension", rule.Dimension),
			zap.Error(err),
			)
		}

		var key string
		switch rule.Dimension {
		case "route":
			key = "route:" + rule.Route
		default:
			key = rule.Dimension	// "ip", "user", "global"
		}

		limiters[key] = limiter
		log.Info("registered limiter",
			zap.String("algorithm", cfg.Algorithm),
			zap.String("dimension", rule.Dimension),
			zap.String("route", rule.Route),
			zap.Int64("limit", int64(rule.Limit)),
			zap.Duration("window", rule.Window),
		)
	}

	
	// --- Router ---
	r := chi.NewRouter()
	r.Use(middleware.RequestID)	 // attaches a unique X-Request-Id to every request
	r.Use(middleware.Recoverer)	// catches panics, returns 500 instead of crashing

	checker := handler.NewChecker(limiters, cfg.Rules, log)

	r.Post("/check", checker.ServerHttp)
	r.Get("/health", handler.HealthHandler)
	r.Get("/ready", handler.ReadyHandler(cbStore))

	
	// --- Server ---
	srv := &http.Server{
		Addr: 		fmt.Sprintf(":%d", cfg.Server.Port),
		Handler:	r,
		ReadTimeout: cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
	}


	// --- Graceful shutdown ---
	// Start server in a goroutine so we can listen for OS signals concurrently.
	go func(){
		log.Info("server starting", zap.String("addr", srv.Addr))
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatal("server error", zap.Error(err))
		}
	}()

	// Block until we receive SIGINT (Ctrl+C) or SIGTERM (kubectl delete pod)
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Info("shutting down...")

	// Give in-flight requests 10 seconds to complete before forcing shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Fatal("forced shutdown", zap.Error(err))
	}

	log.Info("server stopped cleanly")
}