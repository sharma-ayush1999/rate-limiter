# go-ratelimiter

A production-grade, distributed rate limiter service written in Go. Designed to be deployed as a standalone microservice on Kubernetes, with pluggable algorithms, Redis-backed distributed state, and full concurrency safety.

---

---

## Table of Contents

- [Overview](#overview)
- [Features](#features)
- [Rate Limiting Dimensions](#rate-limiting-dimensions)
- [Algorithms](#algorithms)
- [Architecture](#architecture)
- [Concurrency Design](#concurrency-design)
- [Project Structure](#project-structure)
- [Configuration](#configuration)
- [API Reference](#api-reference)
- [Getting Started](#getting-started)
- [Running with Docker](#running-with-docker)
- [Deploying to Kubernetes](#deploying-to-kubernetes)
- [Fault Tolerance](#fault-tolerance)
- [Scalability](#scalability)
- [Testing](#testing)
- [Build Steps](#build-steps)

---

## Overview

`go-ratelimiter` is a standalone HTTP microservice that your upstream services call before serving traffic. It answers one question: **is this request allowed?**

```
Client → Your Service → POST /check [Rate Limiter] → 200 Allow / 429 Deny
                                         ↓
                                       Redis
```

The algorithm is fully configurable via YAML — switching from Token Bucket to Sliding Window requires zero code changes and takes effect immediately on restart (or live reload).

---

## Features

- **5 rate limiting algorithms** — Token Bucket, Fixed Window, Sliding Window Counter, Sliding Window Log, Leaky Bucket
- **Pluggable via Strategy Pattern** — swap algorithms without touching service code
- **Multi-dimensional limiting** — limit by IP, User/API Key, and Endpoint independently
- **Redis-backed distributed state** — all Kubernetes pods share the same counters
- **Concurrency-safe** — atomic Redis operations via Lua scripts; in-process mutex guards
- **Fault tolerant** — configurable fail-open or fail-closed when Redis is unavailable
- **Dockerized** — single `docker-compose up` for local dev
- **Kubernetes-ready** — manifests for Deployment, Service, ConfigMap, HPA
- **Comprehensive tests** — unit tests per algorithm, integration tests with Redis

---

## Rate Limiting Dimensions

Requests can be limited on any combination of the following. Each dimension has its own quota config. A request is **denied if any applicable rule denies it**.

| Dimension | Example Key | Use Case |
| --- | --- | --- |
| IP Address | `ip:203.0.113.42` | Protect against abuse from a single host |
| User / API Key | `user:abc123` | Per-user quotas for authenticated APIs |
| Endpoint / Route | `route:/api/v1/login` | Stricter limits on sensitive endpoints |

Rules are matched in order of specificity: `endpoint+user` > `endpoint+ip` > `user` > `ip` > `global`.

---

## Algorithms

### 1. Token Bucket

Each key gets a bucket with a max capacity of `N` tokens. Tokens refill at a fixed rate. Requests consume one token; if the bucket is empty, the request is denied.

- **Best for:** APIs that allow short bursts but need a sustained rate limit
- **Redis ops:** Lua script (atomic read-modify-write)

### 2. Fixed Window

Counts requests in discrete time windows (e.g., 0–60s, 60–120s). Resets counter at window boundary.

- **Best for:** Simple per-minute or per-hour limits
- **Redis ops:** `INCR` + `EXPIRE` (atomic)
- **Trade-off:** Burst allowed at window boundaries (up to 2× the limit)

### 3. Sliding Window Counter

Approximates a true sliding window using two fixed windows (current + previous) weighted by time elapsed. Efficient and accurate enough for most use cases.

- **Best for:** High-throughput APIs needing smooth limiting without log storage
- **Redis ops:** Two `GET` + Lua for atomic update

### 4. Sliding Window Log

Stores a timestamp log per key in a Redis sorted set. Counts entries within the rolling window on each request.

- **Best for:** Precise rate limiting where exact counts matter
- **Redis ops:** `ZADD` + `ZREMRANGEBYSCORE` + `ZCARD` in a Lua script
- **Trade-off:** Higher memory usage per key

### 5. Leaky Bucket

Requests enter a queue (bucket) and are processed at a fixed output rate. Excess requests that overflow the bucket are denied.

- **Best for:** Smoothing bursty traffic to a constant output rate
- **Redis ops:** Lua script simulating the leak rate

---

## Architecture

```
┌─────────────────────────────────────────────────────┐
│                  go-ratelimiter                      │
│                                                      │
│  ┌──────────┐    ┌────────────┐    ┌─────────────┐  │
│  │  Handler │───▶│  Limiter   │───▶│    Store    │  │
│  │  /check  │    │  (Strategy)│    │  Interface  │  │
│  │  /health │    └────────────┘    └──────┬──────┘  │
│  └──────────┘         │                   │         │
│                  ┌────▼────┐        ┌─────▼──────┐  │
│                  │Algorithm│        │   Redis    │  │
│                  │Interface│        │  Adapter   │  │
│                  └────┬────┘        └────────────┘  │
│           ┌───────────┼───────────┐                 │
│      ┌────▼───┐  ┌────▼───┐ ┌────▼────┐            │
│      │ Token  │  │ Fixed  │ │Sliding  │  ...        │
│      │ Bucket │  │ Window │ │ Window  │             │
│      └────────┘  └────────┘ └─────────┘            │
└─────────────────────────────────────────────────────┘
                          │
                    ┌─────▼──────┐
                    │   Redis    │
                    │  Cluster   │
                    └────────────┘
```

### Strategy Pattern

All algorithms implement a single `RateLimiter` interface:

```go
type RateLimiter interface {
    Allow(ctx context.Context, key string) (bool, error)
    Reset(ctx context.Context, key string) error
    Status(ctx context.Context, key string) (RateStatus, error)
}
```

The active algorithm is selected at startup from config. Swapping it requires only a config change — no code change, no redeployment of dependent services.

---

## Concurrency Design

Concurrency is a first-class concern at every layer.

### Distributed (Cross-Pod) Concurrency

All state mutations in Redis use **Lua scripts** for atomicity. A Lua script executes as a single atomic unit in Redis — no two pods can interleave operations on the same key.

```
Pod A ──┐
        ├──▶ Redis (Lua script executes atomically) ──▶ result
Pod B ──┘
```

Algorithms and their atomic primitives:

| Algorithm | Redis Atomic Mechanism |
| --- | --- |
| Fixed Window | `INCR` + `EXPIRE` |
| Token Bucket | Lua script (read tokens → refill → consume → write) |
| Sliding Window Log | Lua script (`ZADD` + `ZREMRANGE` + `ZCARD`) |
| Sliding Window Counter | Lua script (read two windows → calculate → increment) |
| Leaky Bucket | Lua script (simulate leak → check overflow → write) |

### In-Process Concurrency

- Each algorithm struct is protected by a `sync.RWMutex` for any local (non-Redis) state
- The in-memory store fallback uses a **sharded mutex map** (64 shards) to reduce lock contention under high concurrency
- No shared mutable globals — all state flows through the store interface

### HTTP Layer

Go's `net/http` server handles each request in its own goroutine. The limiter interface is designed to be **goroutine-safe by contract** — documented and enforced.

---

## Project Structure

```
go-ratelimiter/
│
├── cmd/
│   └── server/
│       └── main.go                  # Entrypoint — wires everything together
│
├── internal/
│   ├── algorithm/
│   │   ├── interface.go             # RateLimiter interface + RateStatus type
│   │   ├── token_bucket.go          # Token Bucket implementation
│   │   ├── fixed_window.go          # Fixed Window implementation
│   │   ├── sliding_window_counter.go
│   │   ├── sliding_window_log.go
│   │   ├── leaky_bucket.go
│   │   └── factory.go               # Creates algorithm from config string
│   │
│   ├── store/
│   │   ├── interface.go             # Store interface
│   │   ├── redis.go                 # Redis adapter (go-redis)
│   │   ├── memory.go                # In-memory adapter (sharded, for dev/fallback)
│   │   └── circuit_breaker.go       # Wraps store with fail-open/fail-closed logic
│   │
│   ├── config/
│   │   ├── config.go                # Config structs
│   │   └── loader.go                # YAML loader + env override
│   │
│   ├── handler/
│   │   ├── check.go                 # POST /check handler
│   │   ├── health.go                # GET /health + /ready handlers
│   │   └── middleware.go            # Logging, recovery, request-id middleware
│   │
│   └── keygen/
│       └── keygen.go                # Builds Redis keys from IP/user/route dimensions
│
├── pkg/
│   └── ratelimiter/
│       └── client.go                # Optional Go client for calling this service
│
├── tests/
│   ├── unit/
│   │   ├── token_bucket_test.go
│   │   ├── fixed_window_test.go
│   │   ├── sliding_window_counter_test.go
│   │   ├── sliding_window_log_test.go
│   │   └── leaky_bucket_test.go
│   └── integration/
│       ├── redis_test.go            # Tests against a real Redis (testcontainers)
│       └── concurrent_test.go       # Goroutine-storm tests for race conditions
│
├── k8s/
│   ├── deployment.yaml
│   ├── service.yaml
│   ├── configmap.yaml
│   ├── hpa.yaml                     # Horizontal Pod Autoscaler
│   └── redis/
│       ├── deployment.yaml
│       └── service.yaml
│
├── Dockerfile
├── docker-compose.yml
├── config.yaml                      # Default config (overridden by ConfigMap in K8s)
├── go.mod
├── go.sum
└── README.md
```

---

## Configuration

`config.yaml` controls everything. No code change needed to switch algorithms.

```yaml
server:
  port: 8080
  read_timeout: 5s
  write_timeout: 5s

redis:
  addr: "redis:6379"
  password: ""
  db: 0
  pool_size: 20

algorithm:
  "token_bucket" # token_bucket | fixed_window | sliding_window_counter
  # sliding_window_log | leaky_bucket

fault_tolerance:
  on_redis_failure: "fail_open" # fail_open (allow all) | fail_closed (deny all)

rules:
  - dimension: ip
    limit: 100
    window: 60s

  - dimension: user
    limit: 1000
    window: 60s

  - dimension: route
    route: "/api/v1/login"
    limit: 10
    window: 60s

  - dimension: route
    route: "/api/v1/search"
    limit: 500
    window: 60s
```

---

## API Reference

### `POST /check`

Check whether a request should be allowed.

**Request body:**

```json
{
  "ip": "203.0.113.42",
  "user_id": "user:abc123",
  "route": "/api/v1/login"
}
```

**Response — Allowed (200):**

```json
{
  "allowed": true,
  "remaining": 9,
  "reset_at": "2026-06-13T10:01:00Z"
}
```

**Response — Denied (429):**

```json
{
  "allowed": false,
  "remaining": 0,
  "reset_at": "2026-06-13T10:01:00Z",
  "reason": "route:/api/v1/login exceeded limit of 10 per 60s"
}
```

### `GET /health`

Returns `200 OK` when the service is running.

### `GET /ready`

Returns `200 OK` only when Redis is reachable. Used as K8s readiness probe.

---

## Getting Started

```bash
# 1. Clone and enter the project
git clone https://github.com/yourname/go-ratelimiter
cd go-ratelimiter

# 2. Install dependencies
go mod tidy

# 3. Run Redis locally
docker run -d -p 6379:6379 redis:7-alpine

# 4. Run the service
go run cmd/server/main.go
```

---

## Running with Docker

```bash
# Start both the service and Redis
docker-compose up --build

# Test it
curl -X POST http://localhost:8080/check \
  -H "Content-Type: application/json" \
  -d '{"ip":"1.2.3.4","user_id":"user:42","route":"/api/v1/login"}'
```

`docker-compose.yml` includes:

- `ratelimiter` service (Go binary)
- `redis` service (Redis 7 Alpine)
- Health checks wired up
- Volume for Redis persistence

---

## Deploying to Kubernetes

```bash
# Apply Redis first
kubectl apply -f k8s/redis/

# Apply config
kubectl apply -f k8s/configmap.yaml

# Deploy the service
kubectl apply -f k8s/deployment.yaml
kubectl apply -f k8s/service.yaml

# Enable autoscaling
kubectl apply -f k8s/hpa.yaml
```

The Horizontal Pod Autoscaler (HPA) scales pods based on CPU utilization (target: 70%). Because all state lives in Redis, new pods are instantly effective — no warm-up needed.

---

## Fault Tolerance

| Failure Scenario | Behavior |
| --- | --- |
| Redis unreachable | Configurable: `fail_open` (allow all) or `fail_closed` (deny all) |
| Redis slow (timeout) | Circuit breaker opens after N failures; falls back to policy above |
| Pod crash | Other pods unaffected; state in Redis is unaffected |
| Network partition | Per-pod in-memory fallback (if configured) with short TTL |
| Config error | Service fails fast at startup with a clear error message |

A circuit breaker wraps the Redis store. After a threshold of consecutive failures, it trips open and stops attempting Redis calls for a cooldown period, then half-opens to probe recovery.

---

## Scalability

- **Horizontal scaling** — stateless pods, all state in Redis. Add pods freely.
- **Redis Cluster** — for very high throughput, point the adapter at Redis Cluster. No algorithm changes needed.
- **Sharded in-memory store** — 64-shard mutex map eliminates bottleneck for local dev/single-node deployments
- **HPA** — auto-scales pods under load
- **Connection pooling** — Redis client uses a configurable connection pool (default: 20)

---

## Testing

```bash
# Unit tests (no Redis needed)
go test ./tests/unit/...

# Integration tests (requires Docker for testcontainers)
go test ./tests/integration/...

# Race detector — catches concurrency bugs
go test -race ./...

# All tests with coverage
go test -race -coverprofile=coverage.out ./...
go tool cover -html=coverage.out
```

Integration tests use **testcontainers-go** to spin up a real Redis instance automatically — no manual setup required.

The concurrent test suite fires N goroutines simultaneously against each algorithm to verify no request slips through under race conditions.

---

## Build Steps (Implementation Order)

| Step | What you'll build                                            |
| ---- | ------------------------------------------------------------ |
| 1    | Project scaffold — Go module, folder structure, dependencies |
| 2    | Core `RateLimiter` interface + Strategy pattern + factory    |
| 3    | Redis store abstraction + in-memory fallback                 |
| 4    | Algorithm: Token Bucket (with Lua script)                    |
| 5    | Algorithm: Fixed Window                                      |
| 6    | Algorithm: Sliding Window Counter                            |
| 7    | Algorithm: Sliding Window Log                                |
| 8    | Algorithm: Leaky Bucket                                      |
| 9    | Config system — YAML loader + env overrides                  |
| 10   | HTTP service — `/check`, `/health`, `/ready` handlers        |
| 11   | Multi-dimension key resolution (IP + user + route rules)     |
| 12   | Fault tolerance — circuit breaker + fail-open/closed         |
| 13   | Unit tests + integration tests + race tests                  |
| 14   | Dockerfile + docker-compose                                  |
| 15   | Kubernetes manifests — Deployment, Service, ConfigMap, HPA   |

---

## Dependencies

| Package                                       | Purpose                     |
| --------------------------------------------- | --------------------------- |
| `github.com/redis/go-redis/v9`                | Redis client                |
| `github.com/spf13/viper`                      | Config loading (YAML + env) |
| `github.com/go-chi/chi/v5`                    | HTTP router                 |
| `github.com/testcontainers/testcontainers-go` | Integration test Redis      |
| `github.com/sony/gobreaker`                   | Circuit breaker             |
| `go.uber.org/zap`                             | Structured logging          |
