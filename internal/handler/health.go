package handler

import (
	"context"
	"net/http"
	"time"

	"github.com/sharma-ayush1999/go-ratelimiter/internal/store"
)

// HealthHandler always returns 200. Used as K8s liveness probe.
// If the process is running, it's alive.
func HealthHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}


// ReadyHandler returns 200 only when Redis is reachable.
// Used as K8s readiness probe — keeps the pod out of rotation
// until it can actually serve traffic.
func ReadyHandler(s store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2 * time.Second)
		defer cancel()

		if err := s.Ping(ctx); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"status": "not ready",
				"reason": "redis unreachable",
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	}
}