# Rate Limiter — Deep Dive Q&A

A comprehensive reference covering everything you need to know about rate limiters:
algorithms, trade-offs, Redis, Lua scripts, atomicity, scaling, fault tolerance,
memory, latency, deployment, and availability.

---

## Table of Contents

1. [Fundamentals](#1-fundamentals)
2. [Where to Put a Rate Limiter](#2-where-to-put-a-rate-limiter)
3. [What to Rate Limit On](#3-what-to-rate-limit-on)
4. [Algorithms](#4-algorithms)
5. [Redis & Lua Scripts](#5-redis--lua-scripts)
6. [Atomicity & Race Conditions](#6-atomicity--race-conditions)
7. [Memory & Memory Leaks](#7-memory--memory-leaks)
8. [Scaling](#8-scaling)
9. [Fault Tolerance & Availability](#9-fault-tolerance--availability)
10. [Latency](#10-latency)
11. [Deployment](#11-deployment)
12. [HTTP Headers & Client Communication](#12-http-headers--client-communication)
13. [Edge Cases & Gotchas](#13-edge-cases--gotchas)
14. [Monitoring & Observability](#14-monitoring--observability)
15. [Testing](#15-testing)

---

## 1. Fundamentals

### Q: What is a rate limiter and why do you need one?

A rate limiter controls how many requests a client can make to a service within a time window. Without one:

- A single buggy client can send millions of requests and bring down your service
- Malicious actors can brute-force login endpoints
- One noisy tenant in a multi-tenant system can starve other tenants
- Your downstream dependencies (databases, third-party APIs) get overwhelmed

**Example:** Your login endpoint can safely handle 100 requests/sec. Without a rate limiter, a bot sends 10,000 requests/sec — your database connection pool exhausts, legitimate users can't log in.

---

### Q: What is the difference between rate limiting, throttling, and load shedding?

These are related but distinct:

**Rate Limiting** — Enforces a quota per client over time. Excess requests are rejected with a clear signal (HTTP 429). The client is expected to slow down.
```
Client A: 100 req/min allowed → 101st request → 429 Too Many Requests
```

**Throttling** — Slows down requests rather than rejecting them. The server intentionally introduces delay to reduce throughput. Client waits, not fails.
```
Client A: sending 100 req/sec → server queues them → processes at 10 req/sec
```

**Load Shedding** — The server drops requests to protect itself when it's overwhelmed, regardless of which client sent them. It's server-centric, not client-centric.
```
Server CPU at 95% → start dropping 20% of all incoming requests randomly
```

In practice, a well-designed system uses all three: rate limiting per client, throttling for bursts, and load shedding as a last resort.

---

### Q: What should a rate limiter return when it denies a request?

At minimum:
- **HTTP 429 Too Many Requests** status code
- `Retry-After` header — when the client can safely retry (seconds or HTTP date)
- `X-RateLimit-Limit` — the limit that applies
- `X-RateLimit-Remaining` — requests left in current window
- `X-RateLimit-Reset` — Unix timestamp when the window resets
- A JSON body with a human-readable `reason`

**Example response:**
```http
HTTP/1.1 429 Too Many Requests
Retry-After: 30
X-RateLimit-Limit: 100
X-RateLimit-Remaining: 0
X-RateLimit-Reset: 1718276400
Content-Type: application/json

{
  "allowed": false,
  "reason": "IP 1.2.3.4 exceeded 100 requests per 60s",
  "reset_at": "2026-06-13T10:00:00Z"
}
```

---

## 2. Where to Put a Rate Limiter

### Q: Where in the stack should rate limiting live?

There are four possible locations, each with trade-offs:

```
Internet → [CDN/WAF] → [API Gateway] → [Service Mesh/Sidecar] → [Application] → [Database]
               ①              ②                  ③                    ④
```

**① CDN / WAF (e.g., Cloudflare, AWS WAF)**
- Blocks requests before they hit your infrastructure
- Cheapest per-request — no backend load at all
- Coarse-grained — limited to IP-based rules
- Can't see auth tokens or business logic
- Best for: DDoS protection, geographic blocking

**② API Gateway (e.g., Kong, AWS API Gateway, Nginx)**
- Sees all traffic before it reaches any service
- Can rate limit by IP, API key, user
- Can't access business logic (e.g., "premium users get 10× the limit")
- A single point of failure if not HA
- Best for: global limits across all services, auth-based limits

**③ Service Mesh / Sidecar (e.g., Envoy, Istio)**
- Lives next to each service as a proxy
- Per-service limits — each service manages its own quota
- Consistent implementation without touching app code
- Adds a network hop per request
- Best for: microservice architectures with many teams

**④ Application / Library (what we built)**
- Maximum flexibility — access to full business context
- Can implement "premium user gets more", "admin bypasses limits", etc.
- Must be implemented consistently across every service
- Best for: custom business rules, per-tenant limits

**Recommendation:** Layer them. CDN for DDoS, API Gateway for coarse limits, Application for business-logic-aware limits.

---

### Q: Should the rate limiter be in-process (library) or out-of-process (service)?

**In-process (library embedded in your service):**
```
[Your Service + Rate Limiter Lib] → Redis
```
- Zero network overhead for the check
- Simpler deployment
- Every service must import and configure the library
- Library updates require redeployment of all services

**Out-of-process (standalone service like what we built):**
```
[Your Service] → [Rate Limiter Service] → Redis
```
- One place to update, all services benefit
- Additional network hop (~1-5ms)
- Rate limiter becomes a dependency — if it's down, what happens?
- Can be written in any language, used by any service
- Easier to scale independently

**Rule of thumb:** Start with in-process for simplicity. Move to out-of-process when you have multiple services that all need rate limiting — deduplication of logic pays off.

---

## 3. What to Rate Limit On

### Q: On what basis can you rate limit requests?

The "key" that identifies a rate limit subject. Can be any combination:

**IP Address**
```
key = "rl:ip:203.0.113.42"
```
- Simple, no auth required
- Easy to spoof (VPNs, proxies, botnets with many IPs)
- Mobile users may share IPs via carrier-grade NAT (one IP = thousands of users)
- Good for unauthenticated endpoints

**User ID / API Key**
```
key = "rl:user:user_abc123"
key = "rl:apikey:sk_live_xyz789"
```
- Requires authentication — only works after auth middleware
- Accurate — one user = one quota regardless of IP changes
- Best for authenticated APIs

**Endpoint / Route**
```
key = "rl:route:/api/v1/login"
key = "rl:route:/api/v1/payment"
```
- Different limits per endpoint sensitivity
- /login: 10/min, /search: 1000/min, /payment: 5/min
- Must be combined with IP or user to be per-client

**Composite Keys (most powerful)**
```
key = "rl:route:/api/v1/login:user:abc123"  → per-user per-route
key = "rl:route:/api/v1/search:ip:1.2.3.4"  → per-IP per-route
key = "rl:tenant:acme:user:abc123"           → per-user within tenant
```
- Fine-grained control
- More Redis keys, more memory

**Tenant / Organization**
```
key = "rl:tenant:acme_corp"
```
- Multi-tenant SaaS — each customer gets their own quota
- Premium tenants get higher limits
- One bad tenant can't starve others

**Global**
```
key = "rl:global"
```
- Single shared counter for all traffic
- Last-resort protection for total system capacity
- Blunt instrument — affects all clients equally

---

### Q: What happens when a user is behind a NAT or proxy and shares an IP with thousands of others?

This is a real problem with IP-based limiting. Solutions:

1. **Use auth-based keys** — rate limit on user ID, not IP, for authenticated endpoints
2. **Use `X-Forwarded-For` carefully** — load balancers add the real client IP here, but it can be spoofed
3. **Higher IP limits** — set IP limits high enough that legitimate NAT users aren't affected, rely on user-level limits for precision
4. **Combination keys** — `ip:port` or `ip + user-agent hash` can narrow down individual clients behind NAT

```go
// Extract real IP, respecting proxy headers but with validation
func realIP(r *http.Request) string {
    xff := r.Header.Get("X-Forwarded-For")
    if xff != "" {
        // Take the leftmost IP — the original client
        // (rightmost is the last proxy, which you control)
        parts := strings.Split(xff, ",")
        return strings.TrimSpace(parts[0])
    }
    ip, _, _ := net.SplitHostPort(r.RemoteAddr)
    return ip
}
```

**Warning:** Never trust `X-Forwarded-For` blindly. A malicious client can set it to `"1.2.3.4, 5.6.7.8"` and fake being someone else. Only trust it if your load balancer strips and re-adds it.

---

## 4. Algorithms

### Q: Explain Fixed Window and its boundary burst problem in detail.

Fixed Window divides time into discrete buckets. Each bucket has its own counter.

```
Window: 60s, Limit: 10

Bucket 1: [00:00 → 01:00]  counter = 0..10
Bucket 2: [01:00 → 02:00]  counter = 0..10
```

**The boundary burst problem:**

```
00:58 → 5 requests (in bucket 1)  counter=5  → all allowed
00:59 → 5 requests (in bucket 1)  counter=10 → all allowed
01:00 → 5 requests (in bucket 2)  counter=5  → all allowed ← NEW BUCKET!
01:01 → 5 requests (in bucket 2)  counter=10 → all allowed

Result: 20 requests in 3 seconds — double the intended limit!
```

The hard reset at the window boundary allows a client to effectively double their quota by straddling two windows.

**Implementation:**
```go
// The window is encoded in the key name itself
windowKey = fmt.Sprintf("%s:%d", key, time.Now().Unix()/windowSeconds)
// At t=3661: key = "rl:ip:1.2.3.4:61" (bucket 61)
// At t=3721: key = "rl:ip:1.2.3.4:62" (bucket 62, fresh counter)
```

**When to use it anyway:** When the boundary burst is acceptable. For a "1000 requests/hour" limit, a 2000-request burst over 2 seconds at the hour boundary is usually fine. It's the cheapest algorithm to implement and understand.

---

### Q: How does Token Bucket differ from Leaky Bucket?

They sound similar but model opposite things:

**Token Bucket — controls input rate, allows bursting**

The bucket fills with tokens over time. Requests consume tokens. The bucket can accumulate tokens during quiet periods, enabling bursts later.

```
Capacity=5, Refill=1/sec

Idle for 5 seconds → bucket fills to 5 tokens
Burst of 5 requests → all allowed instantly (tokens consumed)
6th request → denied (bucket empty)
Wait 1 second → 1 token refills → allowed again
```

Think of it as a prepaid credit system. You earn credits over time and spend them in bursts.

**Leaky Bucket — controls output rate, no bursting**

The bucket fills with incoming requests. Requests leak out at a constant rate. If requests fill the bucket faster than it leaks, overflow = denied.

```
Capacity=5, Leak=1/sec

5 requests arrive instantly → bucket=[5/5] full
6th request → overflow → DENIED
1 second later → 1 leaked out → bucket=[4/5]
New request → bucket=[5/5] → ALLOWED
```

Think of it as a queue with a fixed processing rate. Bursts are absorbed into the queue up to capacity, then rejected.

**Key difference:**

| | Token Bucket | Leaky Bucket |
|---|---|---|
| Burst allowed? | Yes, up to capacity | No — processes at constant rate |
| Idle accumulation | Yes — tokens build up | No — empty bucket stays empty |
| Output rate | Variable | Strictly constant |
| Use case | Client-facing APIs | Protecting downstream services |

**Example:** You're calling a payment provider that accepts exactly 10 req/sec. Use Leaky Bucket — you want a strictly constant output rate regardless of how many requests your app generates. If you used Token Bucket, a burst could overwhelm the payment provider even though you technically stayed within quota on average.

---

### Q: Which algorithm has the least memory usage? Which has the most?

From least to most memory per key:

**1. Fixed Window — ~20 bytes per key**
```
Redis string key: "rl:ip:1.2.3.4:28637920" → value: "47" (counter)
= 1 key, 1 integer
```

**2. Sliding Window Counter — ~40 bytes per key (2 counters)**
```
Redis string key current:  "rl:ip:1.2.3.4:28637921" → "47"
Redis string key previous: "rl:ip:1.2.3.4:28637920" → "83"
= 2 keys, 2 integers
```

**3. Token Bucket & Leaky Bucket — ~60 bytes per key (1 hash, 2 fields)**
```
Redis hash: "rl:ip:1.2.3.4" → {tokens: "4", last_refill: "1718276123.456"}
= 1 hash key, 2 float fields
```

**4. Sliding Window Log — O(N) where N = requests in window**
```
Redis sorted set: "rl:ip:1.2.3.4"
  members: [timestamp1, timestamp2, ..., timestampN]
= 1 sorted set, N members × ~50 bytes each
```

For 1 million unique IPs with 100 requests each in a 60s window:
- Fixed Window: 1M × 20B = **20 MB**
- Sliding Window Log: 1M × 100 × 50B = **5 GB**

This is why Sliding Window Log is only for low-traffic, high-precision endpoints.

---

### Q: What is the trade-off between accuracy and performance across algorithms?

```
                 ACCURACY
                    ▲
                    │   Sliding
                    │   Window
                    │   Log ●
                    │
                    │         Token
                    │         Bucket ●
                    │
                    │   Sliding Window
                    │   Counter ●
                    │
                    │         Fixed
                    │         Window ●
                    │
                    └────────────────────► PERFORMANCE (throughput/memory)
```

- **Fixed Window:** Highest performance, lowest accuracy (boundary burst)
- **Sliding Window Counter:** High performance, ~99.9% accuracy
- **Token Bucket / Leaky Bucket:** High performance, exact accuracy
- **Sliding Window Log:** Lower performance (more Redis ops, more memory), exact accuracy

For most production systems: Token Bucket for general APIs, Sliding Window Counter for high-traffic endpoints, Sliding Window Log only for critical low-traffic endpoints.

---

## 5. Redis & Lua Scripts

### Q: Why Redis for rate limiting? Can't we use a regular database?

Redis is ideal because:

**1. Speed** — Redis operates in memory. Sub-millisecond operations vs. 5-50ms for a database query. Rate limit checks add latency to every request — this must be minimal.

**2. Atomic operations** — Redis has built-in atomic commands (`INCR`, `EXPIRE`) and Lua scripting. Databases need transactions which are heavier.

**3. TTL built in** — `EXPIRE key 60` automatically deletes the key after 60 seconds. With a database, you need a background cleanup job.

**4. Data structures** — Redis has sorted sets (`ZADD`, `ZREMRANGEBYSCORE`) which are exactly what Sliding Window Log needs. Databases don't have an equivalent.

**5. Distributed** — Redis can run as a cluster, sharing state across all your pods with no application-level coordination.

**Can you use a database?** Yes, but:
- You'd need transactions for atomicity (slower)
- You'd need a cleanup job for expired records
- Query latency adds up when it's on every request's critical path
- Connection pooling becomes a bottleneck at high concurrency

---

### Q: What is a Lua script in Redis and why do we use it?

Redis executes Lua scripts atomically — the entire script runs as a single unit. No other Redis command from any other client can interleave.

**Without Lua (broken — race condition):**
```
Client A: GET tokens → "1"
Client B: GET tokens → "1"       ← reads before A writes
Client A: SET tokens "0" → allow
Client B: SET tokens "0" → allow  ← both get the last token!

Result: 2 requests allowed, 1 token consumed
```

**With Lua (correct):**
```lua
-- This entire script is atomic
local tokens = tonumber(redis.call("GET", KEYS[1]))
if tokens >= 1 then
    redis.call("SET", KEYS[1], tokens - 1)
    return 1  -- allowed
end
return 0  -- denied
```
```
Client A: executes Lua → reads tokens=1, sets tokens=0 → returns 1 (allowed)
Client B: executes Lua → reads tokens=0 → returns 0 (denied)

Result: exactly 1 request allowed ✓
```

**Key properties of Redis Lua:**
- Atomic — nothing interrupts it
- Runs in Redis's single thread — no parallelism within Redis
- Has access to all Redis commands via `redis.call()`
- `KEYS` = array of key names, `ARGV` = array of arguments
- Return values: numbers become integers, Lua tables become Redis arrays

**Performance:** After the first `EVAL`, Redis caches the script by its SHA-1 hash. Subsequent calls use `EVALSHA` — only the 40-char hash is sent, not the full script.

---

### Q: What Redis commands are used in each algorithm and why?

**Fixed Window:**
```
INCR  key          → atomically increment counter, returns new value
EXPIRE key seconds → set TTL so the key auto-deletes at window end
```
Wrapped in `TxPipeline()` — sent together in one round-trip.

**Token Bucket & Leaky Bucket:**
```
HMGET key field1 field2  → read multiple hash fields in one call
HMSET key f1 v1 f2 v2    → write multiple hash fields atomically
EXPIRE key seconds        → set TTL
```
All inside a Lua script for full atomicity.

**Sliding Window Counter:**
```
GET key                   → read counter value
INCR key                  → increment current window counter
EXPIRE key seconds        → set TTL
```
All inside a Lua script (two GETs + one INCR must be atomic).

**Sliding Window Log:**
```
ZREMRANGEBYSCORE key -inf threshold  → remove old timestamps (sorted set)
ZCARD key                            → count remaining members
ZADD key score member                → add new timestamp
EXPIRE key seconds                   → set TTL
```
All inside a Lua script — the read-then-write must be atomic.

---

### Q: What is EVALSHA and how does it improve performance?

First call: `EVAL script numkeys key1 arg1 ...`
- Redis compiles the Lua script, caches it under its SHA-1 hash
- Executes and returns result

Subsequent calls: `EVALSHA sha1hash numkeys key1 arg1 ...`
- Redis looks up the cached compiled script by hash
- No script transfer over the network
- No recompilation

**Savings:** A Lua script might be 500 bytes. Its SHA-1 hash is always 40 bytes. Under 10,000 req/sec, that's `9,960,000 × 460 bytes = 4.6 GB/sec` of network saved per day.

The go-redis client handles this automatically — it tries `EVALSHA` first and falls back to `EVAL` if the script isn't cached (e.g., after a Redis restart).

---

## 6. Atomicity & Race Conditions

### Q: What race conditions exist in rate limiting and how do we prevent them?

**Race condition 1: Check-Then-Act**
```
Thread A: check(count=4, limit=5) → allowed → [context switch]
Thread B: check(count=4, limit=5) → allowed → increment → count=5
Thread A: [resumes] → increment → count=6 ← exceeded limit!
```
**Fix:** Atomic increment + check in a single operation or Lua script.

**Race condition 2: Read-Modify-Write**
```
Thread A: read tokens=1
Thread B: read tokens=1
Thread A: write tokens=0, return allowed
Thread B: write tokens=0, return allowed ← stole the last token!
```
**Fix:** Lua script — the read-modify-write is one atomic operation.

**Race condition 3: Key expiry timing**
```
Thread A: check → allowed → about to increment
Key expires (TTL=0)
Thread A: increments → creates new key with value 1, no TTL set
Result: key lives forever → memory leak + no rate limiting
```
**Fix:** Always set TTL when creating a key. Our `Increment` uses a pipeline:
```go
pipe.Incr(ctx, key)    // create key with value 1 if not exists
pipe.Expire(ctx, key, ttl)  // set TTL immediately after
```
Both commands execute atomically in the pipeline.

**Race condition 4: Clock skew across pods**
```
Pod A (clock: 10:00:00.000): creates window key "bucket:100"
Pod B (clock: 09:59:59.998): creates window key "bucket:99" ← different bucket!
```
**Fix:** Use Redis server time via `TIME` command, not local pod time, for time-sensitive operations. Or ensure pod clocks are synced via NTP (standard in K8s).

---

### Q: Is Redis itself a single point of contention when all pods talk to it?

Yes — and this is a real concern at very high scale. Redis is single-threaded for command execution.

**Mitigation strategies:**

**1. Redis Pipelining** — Send multiple commands in one network round-trip. Reduces network overhead, not Redis CPU.

**2. Redis Cluster** — Shard keys across multiple Redis nodes. Each node handles a subset of keys. Our key format `rl:ip:1.2.3.4` gets consistently routed to the same shard.

**3. Lua script efficiency** — A Lua script that does 4 Redis operations counts as 1 network round-trip. Reduces the number of client connections blocked waiting.

**4. Local in-process cache** — For very hot keys, cache the result locally for 100ms. Accept slight inaccuracy in exchange for removing Redis from the critical path.

**5. Redis Cluster with read replicas** — Read counters from replicas (slight staleness), write to primary. Trade-off: may allow slightly more requests than the limit due to replication lag.

**Realistic numbers:** A single Redis instance handles ~100,000-500,000 simple operations/sec. With pipelining and Lua scripts, this scales further. For most systems, a single Redis node is sufficient.

---

## 7. Memory & Memory Leaks

### Q: How can a rate limiter cause memory leaks in Redis?

**Leak 1: Keys without TTL**

If your code creates a key but fails to set its TTL (due to a bug or race condition), the key lives forever. With 1 million unique IPs per day, that's 365 million orphan keys per year.

```
// BUG: Increment creates key, but if the service crashes before Expire runs:
redis.INCR("rl:ip:1.2.3.4")  // key created
// crash here
redis.EXPIRE("rl:ip:1.2.3.4", 60)  // never runs → key lives forever
```

**Fix:** Use a pipeline or Lua script that always sets TTL atomically with the write.

**Leak 2: Sliding Window Log growth**

Each request adds one member to the sorted set. If TTL is too short, old members aren't cleaned up by `ZREMRANGEBYSCORE`. If TTL is too long, memory balloons.

**Fix:** Set TTL to `window_duration + 1s`. The `ZREMRANGEBYSCORE` in the Lua script handles cleanup, and TTL is a safety net.

**Leak 3: Forgot to account for key cardinality**

If you rate limit on `user_id:endpoint` and have 1M users × 100 endpoints = 100M keys. Even at 100 bytes each = 10 GB just for rate limit state.

**Fix:** Only rate limit on combinations you actually need. Use `ip` and `user` as primary keys. Add `route` only for high-value sensitive endpoints.

**Leak 4: Stale keys from decommissioned users**

Users cancel their accounts, but their rate limit keys persist until TTL expires.

**Fix:** Set TTL = max window duration. These keys will naturally expire. Don't bother explicitly deleting them.

---

### Q: How do you calculate the memory footprint of your rate limiter?

**Formula:**
```
Memory = unique_keys × memory_per_key × keys_per_entity
```

**Fixed Window:**
```
1M unique IPs × 20 bytes × 1 key = 20 MB
```

**Token Bucket:**
```
1M unique IPs × 60 bytes × 1 key = 60 MB
```

**Sliding Window Log:**
```
unique_keys × (requests_per_window × 50 bytes)
1M IPs × (100 requests × 50 bytes) = 5 GB ← not viable for this scale
```

**Redis memory overhead:** Each key has ~50 bytes of Redis internal overhead beyond the value. For millions of small keys, this overhead dominates.

**Monitoring:** Use Redis `INFO memory` and `DBSIZE` to track key count and memory usage. Set `maxmemory` in Redis config with `allkeys-lru` eviction policy as a safety net — Redis will evict least-recently-used keys if memory fills up.

```
maxmemory 1gb
maxmemory-policy allkeys-lru
```

With `allkeys-lru`, Redis prefers to evict rate limit keys for inactive clients — exactly what you want. Active clients keep their keys warm.

---

## 8. Scaling

### Q: How does rate limiting work when you have multiple instances of your service?

Without shared state, each pod has its own counter — 3 pods = 3× the intended limit:

```
Pod 1: user X has made 80/100 requests
Pod 2: user X has made 80/100 requests  ← different counter!
Pod 3: user X has made 80/100 requests

Reality: user X has made 240 requests → 2.4× the limit
```

**Solution: Centralized Redis**

All pods share one Redis instance. Every increment hits the same counter.

```
Pod 1 ─┐
Pod 2 ──┼──► Redis: "rl:user:X" = 80
Pod 3 ─┘

Pod 1 increments → 81
Pod 2 increments → 82 (read after Pod 1's write)
Pod 3 increments → 83
```

Redis's single-threaded command execution ensures correctness even with concurrent pods.

---

### Q: What is the "two-phase commit" approach to distributed rate limiting without Redis?

If you can't use Redis, you can approximate distributed rate limiting using a gossip protocol or token sharing:

**Approach 1: Sticky sessions (simple but limited)**
Route all requests from a user to the same pod (via consistent hashing at the load balancer). Each pod limits independently. Works until a pod restarts.

**Approach 2: Periodic sync**
Each pod tracks local counts and periodically syncs with a central store (every 100ms). Accept that between syncs, each pod allows `limit` requests — total actual requests could be `limit × num_pods × sync_interval / window`.

**Approach 3: Distributed token distribution**
A central "token server" periodically distributes token batches to pods. Each pod consumes from its local batch. Less accurate but reduces central store pressure.

None of these match the accuracy of centralized Redis. Use Redis.

---

### Q: How do you rate limit at massive scale (millions of req/sec)?

At extreme scale (think Twitter, Stripe), a single Redis isn't enough. Strategies:

**1. Redis Cluster**
Shard keys across 6-100+ Redis nodes. Keys are deterministically routed by hash slot. Our key format works perfectly — each user/IP maps to one shard.

```
hash_slot = CRC16("rl:ip:1.2.3.4") % 16384 → node 3
hash_slot = CRC16("rl:user:abc")   % 16384 → node 7
```

**2. Local approximate limiting first**
Check a local in-memory counter (cheap, no network) before hitting Redis. If the local counter alone exceeds a threshold, deny immediately without touching Redis.

```go
localCount := atomic.AddInt64(&localCounters[key], 1)
if localCount > localThreshold {
    return RateStatus{Allowed: false} // deny without Redis
}
// Only hit Redis if local check passes
return redisLimiter.Allow(ctx, key)
```

This gives Redis a ~90% reduction in traffic while being slightly permissive.

**3. Token pre-allocation**
Each pod fetches a batch of tokens (e.g., 100) from Redis in one call, then serves 100 requests locally. Reduces Redis calls by 100×. Trade-off: a pod crash wastes unused tokens.

```
Pod 1: fetch 100 tokens from Redis → serve 100 requests locally
Pod 2: fetch 100 tokens from Redis → serve 100 requests locally
```

**4. Probabilistic limiting**
Instead of exactly tracking every request, sample a percentage of requests. Check rate limit for 10% of traffic, extrapolate for the rest. Very approximate but scales infinitely.

---

## 9. Fault Tolerance & Availability

### Q: What happens when Redis goes down? What are fail-open and fail-closed?

When Redis is unreachable, every rate limit check returns an error. You have two options:

**Fail Open — allow all requests during outage**
```go
if err != nil && isRedisDown(err) {
    return RateStatus{Allowed: true}  // let everything through
}
```
- Pros: Users aren't affected by an infrastructure problem they didn't cause
- Cons: During an outage, rate limiting is completely disabled — bad actors can abuse this
- Best for: User-facing APIs where availability > security

**Fail Closed — deny all requests during outage**
```go
if err != nil && isRedisDown(err) {
    return RateStatus{Allowed: false, Reason: "service temporarily unavailable"}
}
```
- Pros: Maintains security properties even during outage
- Cons: Legitimate users get 429s despite not exceeding their limit
- Best for: Financial services, security-critical endpoints

**Fail to local in-memory limiter**
A third option: fall back to a per-pod in-memory rate limiter when Redis is down. Less accurate (each pod has its own limit) but functional.

```go
if err != nil && isRedisDown(err) {
    return localLimiter.Allow(ctx, key)  // in-memory fallback
}
```

---

### Q: What is a circuit breaker and why does it belong in a rate limiter?

Without a circuit breaker, when Redis is slow (not fully down):
```
Request → check rate limit → wait 30s for Redis timeout → 429 or allow
```
Every request waits 30 seconds. Your rate limiter is now the bottleneck.

A circuit breaker monitors failure rate and "trips open" when failures exceed a threshold — stopping all Redis calls immediately:

```
CLOSED (normal) → [5 consecutive failures] → OPEN (fail fast) → [10s cooldown] → HALF-OPEN → [1 success] → CLOSED
```

**States:**
- **Closed:** All calls go to Redis. Track failures.
- **Open:** Don't call Redis at all. Return fail-open/closed immediately. No timeout wait.
- **Half-open:** After cooldown, allow 1 probe request. If it succeeds → closed. If it fails → open again.

**Result:** During a Redis outage, your rate limiter adds <1ms latency (fail-fast) instead of 30s (timeout). 99.9th percentile latency is protected.

**Our implementation uses `gobreaker`:**
```go
settings := gobreaker.Settings{
    MaxRequests: 1,              // 1 probe in half-open
    Timeout:     10 * time.Second, // stay open for 10s
    ReadyToTrip: func(counts gobreaker.Counts) bool {
        return counts.ConsecutiveFailures >= 5
    },
}
```

---

### Q: How do you make Redis itself highly available for rate limiting?

**Option 1: Redis Sentinel**
```
[Primary] ←── replication ──► [Replica 1]
                              [Replica 2]
        ↑
   [Sentinel 1]  [Sentinel 2]  [Sentinel 3]  ← vote on failover
```
Sentinel monitors the primary. On failure, promotes a replica. Failover takes 5-30 seconds — during which rate limiting is unavailable (circuit breaker handles this).

**Option 2: Redis Cluster**
```
[Node 1: slots 0-5460]     [Node 4: replica of Node 1]
[Node 2: slots 5461-10922] [Node 5: replica of Node 2]
[Node 3: slots 10923-16383][Node 6: replica of Node 3]
```
Automatic sharding and failover. A node failure only affects its hash slots. Other nodes keep working. For rate limiting this means only some keys are temporarily unavailable — circuit breaker handles those specific failures.

**Option 3: Read from replica (async)**
For read-heavy operations, read from replicas. Write (increment) to primary. Replication lag means replicas might be 1-10ms behind — could allow slightly more than the limit during lag. Acceptable for most use cases.

---

## 10. Latency

### Q: How much latency does rate limiting add to each request?

**Breakdown for a typical request:**

```
Request arrives → check rate limit → process request → return response
                  ↑
              This is the added cost
```

| Component | Latency |
|---|---|
| Go function call overhead | ~100ns |
| Redis round-trip (local network) | 0.5-2ms |
| Redis command execution (Lua) | 0.1-0.5ms |
| **Total added latency** | **1-3ms** |

For services with p99 latency of 50-200ms, 1-3ms is acceptable (1-6% overhead).

**Optimization strategies:**

**1. Connection pooling** — Never create a new Redis connection per request. Our `RedisStore` uses go-redis's built-in connection pool (`PoolSize: 20` in config).

**2. Lua scripts** — 4 Redis operations in 1 network round-trip instead of 4 separate round-trips.
```
Without Lua: 4 ops × 1ms = 4ms
With Lua:    1 round-trip × 1ms = 1ms  (3× faster)
```

**3. Async rate limiting** — Check rate limit in parallel with starting request processing:
```go
resultCh := make(chan RateStatus, 1)
go func() {
    status, _ := limiter.Allow(ctx, key)
    resultCh <- status
}()

// Start processing while rate limit check runs
// ...
status := <-resultCh
if !status.Allowed {
    return 429
}
```
Only valid if processing is idempotent and can be cancelled.

**4. Local cache** — Cache the result of `Allow()` for 10-100ms per key. Accept slight over-allowance.

---

### Q: What is the latency impact of Lua scripts vs. simple commands?

**Simple command pipeline (Fixed Window):**
```
Client → [INCR + EXPIRE pipeline] → Redis → Client
Network: 1 round-trip
Redis CPU: 2 operations
Total: ~1ms
```

**Lua script (Token Bucket):**
```
Client → [EVALSHA script] → Redis executes 4+ ops → Client
Network: 1 round-trip  ← same as pipeline
Redis CPU: 4+ operations
Total: ~1.5-2ms
```

Lua is ~50% slower per request than a simple pipeline due to more Redis-side operations. But it provides atomicity that a pipeline cannot. For rate limiting, the correctness guarantee is worth the extra 0.5ms.

---

## 11. Deployment

### Q: How should a rate limiter be deployed in Kubernetes?

**Architecture:**
```
[Ingress] → [Service A pods] → [Rate Limiter pods] → [Redis]
            [Service B pods] ────────────────────────────────┤
            [Service C pods] ────────────────────────────────┘
```

**Deployment considerations:**

**1. Replicas** — Run at least 3 replicas. Rate limiter is on the critical path — a pod restart during high traffic matters.

**2. Pod Disruption Budget** — Ensure at least 2 pods are always available during deployments:
```yaml
apiVersion: policy/v1
kind: PodDisruptionBudget
spec:
  minAvailable: 2
  selector:
    matchLabels:
      app: rate-limiter
```

**3. Resource limits** — Rate limiter is CPU-bound (Go + JSON parsing). Set appropriate requests/limits:
```yaml
resources:
  requests:
    cpu: "100m"
    memory: "64Mi"
  limits:
    cpu: "500m"
    memory: "128Mi"
```

**4. Liveness vs. Readiness probes:**
```yaml
livenessProbe:
  httpGet:
    path: /health   # process alive?
  initialDelaySeconds: 5

readinessProbe:
  httpGet:
    path: /ready    # Redis reachable?
  initialDelaySeconds: 5
```
When Redis is down, `/ready` returns 503 → pod removed from service → requests go to healthy pods → circuit breaker on those pods handles the Redis outage.

**5. HPA (Horizontal Pod Autoscaler):**
```yaml
spec:
  minReplicas: 3
  maxReplicas: 20
  metrics:
  - type: Resource
    resource:
      name: cpu
      target:
        type: Utilization
        averageUtilization: 70
```

---

### Q: Should Redis run inside the cluster or outside?

**Inside the cluster (Redis in K8s):**
- Lower network latency (pod-to-pod)
- Easier to manage with Helm charts (e.g., Bitnami Redis)
- Risk: K8s disruptions (node failure, upgrade) can affect Redis
- Use persistent volumes for Redis data

**Outside the cluster (managed Redis):**
- AWS ElastiCache, GCP Memorystore, Azure Cache for Redis
- High availability, automatic backups, failover — managed for you
- Slightly higher network latency (still <2ms within a region)
- Higher cost
- Best for production

**Recommendation:** Use a managed Redis service in production. Use Redis in K8s (or Docker) for local development and staging.

---

## 12. HTTP Headers & Client Communication

### Q: What standard HTTP headers should a rate limiter set?

**On every response (allowed or denied):**
```http
X-RateLimit-Limit: 100
X-RateLimit-Remaining: 47
X-RateLimit-Reset: 1718276400
```

**On denied responses only:**
```http
HTTP/1.1 429 Too Many Requests
Retry-After: 30
X-RateLimit-Limit: 100
X-RateLimit-Remaining: 0
X-RateLimit-Reset: 1718276400
```

`Retry-After` can be either:
- Seconds until retry: `Retry-After: 30`
- HTTP date: `Retry-After: Mon, 13 Jun 2026 10:01:00 GMT`

**Draft standard — RateLimit headers (IETF):**
```http
RateLimit-Limit: 100
RateLimit-Remaining: 47
RateLimit-Reset: 30  ← seconds until reset (not Unix timestamp)
RateLimit-Policy: 100;w=60  ← 100 requests per 60s window
```

---

### Q: How should clients handle rate limit responses?

Good client behavior:

**1. Respect `Retry-After`** — Don't retry immediately. Wait the specified time.

**2. Exponential backoff with jitter** — If `Retry-After` isn't set or you're unsure:
```go
func retryDelay(attempt int) time.Duration {
    base := time.Second
    max := 30 * time.Second
    delay := base * (1 << attempt)  // 1s, 2s, 4s, 8s, 16s, 30s
    if delay > max {
        delay = max
    }
    // Add jitter: ±25% of delay
    jitter := time.Duration(rand.Float64() * float64(delay) * 0.25)
    return delay + jitter
}
```

**3. Watch `X-RateLimit-Remaining`** — Slow down proactively before hitting 0:
```go
if remaining < 10 {
    time.Sleep(100 * time.Millisecond)  // self-throttle
}
```

**4. Don't retry on 429 in a tight loop** — This makes the problem worse and may get your IP blocked permanently.

---

## 13. Edge Cases & Gotchas

### Q: What are the most common bugs in rate limiter implementations?

**Bug 1: Off-by-one in the limit check**
```go
// Wrong: allows limit+1 requests
if count < limit {
    count++
    return allowed
}

// Correct: allows exactly limit requests
if count <= limit {  // count was incremented before this check
    return allowed
}

// Or equivalently:
count = increment()
if count > limit {
    return denied
}
```

**Bug 2: Forgetting to set TTL**
```go
// Bug: key lives forever if Expire fails
redis.INCR(key)
redis.EXPIRE(key, 60)  // What if this fails?

// Fix: Lua script or pipeline ensures both run atomically
```

**Bug 3: Using local time instead of Redis time**
Pods in a cluster may have clock drift of 10-100ms. For Fixed Window, this means different pods may be in different windows:
```
Pod A (clock: 10:00:59.999): window key = "bucket:999"
Pod B (clock: 10:01:00.001): window key = "bucket:1000"  ← different bucket!
```
Fix: Use `redis.TIME` command for the authoritative timestamp.

**Bug 4: Not handling Redis Nil as zero**
When a key doesn't exist, Redis returns Nil, not "0". If you don't handle this:
```go
count, err := client.Get(ctx, key).Int64()
// err == redis.Nil when key doesn't exist
// If you return error, you incorrectly deny the first request!
if err == redis.Nil {
    return 0, nil  // treat missing key as 0
}
```

**Bug 5: Sliding Window Log member collision**
Using milliseconds as the sorted set member:
```
t=1000ms: ZADD key 1000 1000 → {1000: score=1000}
t=1000ms: ZADD key 1000 1000 → updates existing member (same member!)
```
Result: two simultaneous requests, only one entry in the set → one request untracked → allowed when it shouldn't be. Fix: use microseconds or append a unique ID.

---

### Q: How do you handle distributed rate limiting when requests are routed through multiple data centres?

**Problem:** User sends requests to data centre A and data centre B. Each DC has its own Redis. The combined request count exceeds the limit but each DC independently allows them.

**Solutions:**

**1. Single global Redis** — All DCs point to one Redis cluster. Adds cross-DC latency (50-200ms). Unacceptable for most APIs.

**2. Active-Active Redis with CRDT** — Redis Enterprise supports conflict-free replicated data types (CRDTs). Counters automatically sync across DCs with eventual consistency. May slightly exceed limit during sync window.

**3. Regional limits** — Give each region its own limit: global limit = 100, each of 2 regions = 60. Users can't easily exceed the global limit without being in both regions simultaneously.

**4. Sticky routing** — Route each user to one DC consistently (via geoDNS or consistent hashing). That DC's Redis is the source of truth. Fails over to another DC if the primary is down.

**Recommendation:** For most systems, regional limits are the simplest correct solution. True global accuracy across DCs requires accepting either latency (global Redis) or eventual consistency (CRDT).

---

## 14. Monitoring & Observability

### Q: What metrics should a rate limiter emit?

**Request metrics:**
```
rate_limiter_requests_total{result="allowed", dimension="ip"}
rate_limiter_requests_total{result="denied", dimension="user"}
rate_limiter_requests_total{result="error", algorithm="token_bucket"}
```

**Latency metrics:**
```
rate_limiter_check_duration_seconds{quantile="0.5"}   → median latency
rate_limiter_check_duration_seconds{quantile="0.99"}  → p99 latency
```

**Circuit breaker metrics:**
```
rate_limiter_circuit_breaker_state{state="open"}    → 1 when open
rate_limiter_circuit_breaker_trips_total            → total trips
```

**Redis metrics (from Redis INFO):**
```
redis_connected_clients
redis_used_memory_bytes
redis_keyspace_hits_total
redis_keyspace_misses_total
redis_commands_processed_total
```

**Alerting rules:**
- Deny rate > 10% of total requests → alert
- p99 latency > 10ms → alert
- Circuit breaker open for > 30s → page
- Redis memory > 80% of maxmemory → alert

---

### Q: How do you debug a rate limiter in production?

**1. Check what's in Redis:**
```bash
redis-cli --scan --pattern "rl:ip:*" | head -20  # list rate limit keys
redis-cli TTL "rl:ip:1.2.3.4"                    # check TTL
redis-cli GET "rl:ip:1.2.3.4"                    # check counter
redis-cli HGETALL "rl:ip:1.2.3.4"               # check hash (token bucket)
redis-cli ZCARD "rl:ip:1.2.3.4"                  # check sorted set size (SW log)
```

**2. Check key count:**
```bash
redis-cli DBSIZE          # total keys
redis-cli INFO memory     # memory usage
redis-cli INFO keyspace   # keys per DB
```

**3. Monitor commands in real time:**
```bash
redis-cli MONITOR  # prints every Redis command — use sparingly in production!
```

**4. Request ID tracing:** Our `middleware.RequestID` adds `X-Request-Id` to every request. Log it at the rate limiter with the decision and the key — trace a specific denied request end-to-end.

---

## 15. Testing

### Q: How do you test a rate limiter correctly?

**Unit tests — test each algorithm in isolation using MemoryStore:**
```go
func TestTokenBucket_AllowsDeniesCorrectly(t *testing.T) {
    s := store.NewMemoryStore()
    limiter := algorithm.NewTokenBucket(s, algorithm.TokenBucketConfig{
        Capacity:   5,
        RefillRate: 1.0,
        Window:     time.Minute,
    })

    // First 5 should be allowed
    for i := 0; i < 5; i++ {
        status, err := limiter.Allow(context.Background(), "test-key")
        require.NoError(t, err)
        assert.True(t, status.Allowed, "request %d should be allowed", i+1)
    }

    // 6th should be denied
    status, err := limiter.Allow(context.Background(), "test-key")
    require.NoError(t, err)
    assert.False(t, status.Allowed)
}
```

**Concurrency tests — verify no race conditions:**
```go
func TestTokenBucket_Concurrent(t *testing.T) {
    s := store.NewMemoryStore()
    limiter := algorithm.NewTokenBucket(s, algorithm.TokenBucketConfig{
        Capacity: 100, RefillRate: 1.0, Window: time.Minute,
    })

    var allowed, denied int64
    var wg sync.WaitGroup

    // Fire 200 goroutines simultaneously
    for i := 0; i < 200; i++ {
        wg.Add(1)
        go func() {
            defer wg.Done()
            status, _ := limiter.Allow(context.Background(), "concurrent-key")
            if status.Allowed {
                atomic.AddInt64(&allowed, 1)
            } else {
                atomic.AddInt64(&denied, 1)
            }
        }()
    }

    wg.Wait()
    assert.Equal(t, int64(100), allowed, "exactly 100 should be allowed")
    assert.Equal(t, int64(100), denied,  "exactly 100 should be denied")
}
```

Run with race detector:
```bash
go test -race ./...
```

**Integration tests — test against real Redis:**
```go
func TestFixedWindow_WithRealRedis(t *testing.T) {
    // testcontainers spins up a real Redis in Docker for this test
    ctx := context.Background()
    container, _ := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
        ContainerRequest: testcontainers.ContainerRequest{
            Image:        "redis:7-alpine",
            ExposedPorts: []string{"6379/tcp"},
            WaitingFor:   wait.ForListeningPort("6379/tcp"),
        },
        Started: true,
    })
    defer container.Terminate(ctx)

    host, _ := container.Host(ctx)
    port, _ := container.MappedPort(ctx, "6379")

    s, _ := store.NewRedisStore(store.RedisOptions{
        Addr: fmt.Sprintf("%s:%s", host, port.Port()),
    })

    limiter := algorithm.NewFixedWindow(s, algorithm.FixedWindowConfig{
        Limit: 3, Window: time.Minute,
    })

    for i := 0; i < 3; i++ {
        status, _ := limiter.Allow(ctx, "integration-key")
        assert.True(t, status.Allowed)
    }
    status, _ := limiter.Allow(ctx, "integration-key")
    assert.False(t, status.Allowed)
}
```

**Load tests — verify performance under pressure:**
```bash
# Use hey or wrk to hammer the /check endpoint
hey -n 100000 -c 100 -m POST \
  -H "Content-Type: application/json" \
  -d '{"ip":"1.2.3.4","route":"/api/test"}' \
  http://localhost:8080/check
```

Key things to verify in load tests:
- p99 latency < 5ms under load
- Deny count matches expected (exactly `limit` requests allowed per window)
- No goroutine leaks (`runtime.NumGoroutine()` stays stable)
- Redis connection pool doesn't exhaust

---

## Quick Reference

### Algorithm Selection Guide

```
Is precision critical (payments, auth)?
├── Yes → Sliding Window Log
└── No
    ├── High traffic (>10K req/sec per key)?
    │   └── Sliding Window Counter
    └── Normal traffic
        ├── Need to allow bursting?
        │   └── Token Bucket
        ├── Protecting downstream at constant rate?
        │   └── Leaky Bucket
        └── Simple coarse limits (hourly/daily)?
            └── Fixed Window
```

### Failure Mode Decision Tree

```
Redis unreachable
├── Security critical (payments, auth)?
│   └── Fail Closed → deny all
└── User-facing API?
    └── Fail Open → allow all
        └── Or: fall back to local in-memory limiter
```

### Deployment Checklist

- [ ] Redis HA configured (Sentinel or Cluster)
- [ ] Circuit breaker wrapping Redis store
- [ ] Fail policy explicitly chosen (open vs. closed)
- [ ] TTL set on every Redis key write
- [ ] `/health` (liveness) and `/ready` (readiness) probes configured
- [ ] HPA configured for rate limiter pods
- [ ] `X-RateLimit-*` headers on every response
- [ ] `Retry-After` header on 429 responses
- [ ] Metrics emitted (request count, latency, deny rate)
- [ ] Alerts configured (high deny rate, p99 latency, circuit breaker)
- [ ] Rate limiter tested with `-race` flag
- [ ] Integration tests run against real Redis
