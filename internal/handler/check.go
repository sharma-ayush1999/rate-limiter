package handler

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/sharma-ayush1999/go-ratelimiter/internal/algorithm"
	"github.com/sharma-ayush1999/go-ratelimiter/internal/config"
	"github.com/sharma-ayush1999/go-ratelimiter/internal/keygen"
	"go.uber.org/zap"
)

const prefix = "rl"

// CheckRequest is the JSON body sent by upstream services to /check.
type CheckRequest struct {
	IP		string	`json:"ip"`
	UserID	string	`json:"user_id"`
	Route	string	`json:"route"`
}

// CheckResponse is the JSON response returned by /check.
type CheckResponse struct {
	Allowed		bool 		`json:"allowed"`
	Remaining	int64		`json:"remaining"`
	ResetAt		time.Time	`json:"resetAt"`
	Reason		string		`json:"reason"`
}


// Checker holds the per-rule limiters and the global config.
// One Checker is created at startup and shared across all requests.
type Checker struct {
	// limiters maps dimension+route → RateLimiter
	// e.g. "ip" → TokenBucket, "route:/api/v1/login" → TokenBucket
	limiters	map[string]algorithm.RateLimiter
	rules 		[]config.RuleConfig
	log			*zap.Logger
}


func NewChecker(
	limiters map[string]algorithm.RateLimiter,
	rules []config.RuleConfig,
	log *zap.Logger,
) *Checker {
	return &Checker{
		limiters: limiters,
		rules: rules,
		log: log,
	}
}


func (c *Checker) ServerHttp(w http.ResponseWriter, r *http.Request) {
	// Decode request body
	var req CheckRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "invalid request body",
		})
		return
	}
	// Check all applicable rules in order.
	// First denial wins — we stop and return immediately.
	for _, rule := range c.rules {
		limiter, key, ok := c.resolveRule(rule, req)
		if !ok {
			// Rule doesn't apply to this request (e.g. route rule for a different route)
			continue
		}

		status, err := limiter.Allow(r.Context(), key)
		if err != nil {
			c.log.Error("limiter error", zap.String("key", key), zap.Error(err))
			// Fault tolerance is handled by the circuit breaker (Step 12).
			// If we reach here the circuit breaker has already decided — treat as allow.
			continue
		}

		if !status.Allowed {
			writeJSON(w, http.StatusTooManyRequests, CheckResponse{
				Allowed: false,
				Remaining: status.Remaining,
				ResetAt: status.ResetAt,
				Reason: status.Reason,
			})
			return
		}
	}
}

// resolveRule determines whether a rule applies to this request,
// and if so returns the limiter and the Redis key to use.
func (c *Checker) resolveRule(
	rule config.RuleConfig,
	req CheckRequest,
) (algorithm.RateLimiter, string, bool) {
	switch rule.Dimension{
	case "ip":
		if req.IP == "" {
			return nil, "", false
		}
		limiter, ok := c.limiters["ip"]
		return limiter, keygen.FromIP(req.IP), ok
	case "user":
		if req.UserID == "" {
			return nil, "", false
		}
		limiter, ok := c.limiters["user"]
		return limiter, keygen.FromUser(req.UserID), ok
	case "route":
		if req.Route == "" || req.Route != rule.Route {
			return nil, "", false
		}
		limiterKey := "route:" + rule.Route
		limiter, ok := c.limiters[limiterKey]
		return limiter, keygen.FromRoute(req.Route), ok
	case "global":
		limiter, ok := c.limiters["global"]
		return limiter, prefix + ":global", ok
	default:
		return nil, "", false
	}
}


func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}