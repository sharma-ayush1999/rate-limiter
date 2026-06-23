# ── Stage 1: Build ────────────────────────────────────────────────────────────
# Use the official Go image to compile the binary.
# "builder" is just a name — referenced in Stage 2.
FROM golang:1.25.0-alpine AS builder

# Install git — needed by go mod download for some dependencies.
RUN apk add --no-cache git

# Set working directory inside the container.
WORKDIR /app

# Copy dependency files first — Docker caches this layer separately.
# If go.mod and go.sum haven't changed, Docker skips re-downloading modules.
# This is the most important caching trick for Go Docker builds.
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the source code.
COPY . .

# Build the binary.
# CGO_ENABLED=0  → static binary (no C runtime dependency)
# GOOS=linux     → target Linux (even if building on macOS)
# -ldflags="-s -w" → strip debug info and symbol table → smaller binary
# -o /app/server  → output path
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w" \
    -o /app/server \
    ./cmd/server

# ── Stage 2: Runtime ──────────────────────────────────────────────────────────
# Start from a minimal image — no Go toolchain, no source code.
# "scratch" is completely empty (3MB total). "alpine" (5MB) is used here
# because it includes CA certificates (needed for HTTPS calls) and a shell
# (useful for exec-ing into the container to debug).
FROM alpine:3.19

# Security: don't run as root.
# Create a non-root user and group named "appuser".
RUN addgroup -S appuser && adduser -S appuser -G appuser

WORKDIR /app

# Copy only the compiled binary from Stage 1 — nothing else.
# The Go source, test files, and build tools are left behind.
COPY --from=builder /app/server .

# Copy the default config — can be overridden by K8s ConfigMap.
COPY config.yaml .

# Switch to non-root user.
USER appuser

# Expose the port the service listens on.
# This is documentation only — it doesn't actually publish the port.
EXPOSE 8080

# Health check — Docker will mark the container unhealthy if this fails.
# Runs every 30s, times out after 5s, allows 3 failures before unhealthy.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -q0- http://localhost:8080/health || exit 1


# Run the binary.
# Use exec form (JSON array) not shell form — exec form receives signals
# directly (SIGTERM for graceful shutdown). Shell form wraps in /bin/sh
# which eats the signal.
ENTRYPOINT ["/app/server"]