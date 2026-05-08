# Advanced Synchronization Primitives

**Production-grade Go synchronization primitives — built from atomics, visualized in real-time, monitored with Prometheus.**

[![Go Version](https://img.shields.io/badge/go-1.21-blue.svg)](https://golang.org/dl/)
[![CI](https://github.com/sanskarpan/advanced-synchronization-primitives/actions/workflows/ci.yml/badge.svg)](https://github.com/sanskarpan/advanced-synchronization-primitives/actions)
[![Coverage](https://img.shields.io/badge/coverage-%E2%89%A570%25-brightgreen.svg)](https://github.com/sanskarpan/advanced-synchronization-primitives)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Go Report Card](https://goreportcard.com/badge/github.com/sanskar/syncprimitives)](https://goreportcard.com/report/github.com/sanskar/syncprimitives)

---

## What This Is

This library implements eight fundamental synchronization primitives entirely from atomic operations and custom waiter queues — no `sync.Mutex` in the hot paths, no channel-based locks. Each primitive ships with:

- Full statistics instrumentation (acquires, releases, wait times, contention histograms)
- Context-cancellable variants for every blocking operation
- A real-time WebSocket dashboard that visualizes goroutine state transitions live
- Prometheus-compatible `/metrics` endpoint with per-primitive histograms
- JSON snapshot persistence across server restarts
- TLS, CORS, per-connection rate limiting, and connection caps out of the box

---

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                        CLIENT LAYER                             │
│  Browser Dashboard (Canvas + WebSocket JS)                      │
│  Prometheus Scraper   CLI (future)   Go SDK (future)            │
└───────────────────────────┬─────────────────────────────────────┘
                            │ WebSocket / HTTP
┌───────────────────────────▼─────────────────────────────────────┐
│                     WEB SERVER LAYER  (web/)                    │
│                                                                 │
│  NewServerWithConfig(cfg)                                       │
│  ├── HandleWebSocket   — upgrades, auth, rate-limit, dispatch  │
│  ├── HandleMetrics     — /metrics  Prometheus text format      │
│  ├── HandleHealthz     — /healthz  uptime + dropped_broadcasts │
│  ├── HandleReadyz      — /readyz   lightweight readiness       │
│  └── HandleStatic      — /         embedded dashboard SPA      │
│                                                                 │
│  Per-Connection State: connState{msgTimes, mu}                  │
│  Connection Cap: MaxConns (default 1000)                        │
│  Snapshot persistence: SnapshotPath (optional JSON file)        │
└──────────┬──────────────────────────┬───────────────────────────┘
           │                          │
┌──────────▼──────────┐  ┌────────────▼──────────────────────────┐
│  SCHEDULER LAYER    │  │         METRICS LAYER                  │
│  (internal/         │  │         (internal/metrics/)            │
│   scheduler/)       │  │                                        │
│                     │  │  MetricsCollector                      │
│  Scheduler          │  │  ├── RegisterPrimitive(id, type, name) │
│  ├── primitives map │  │  ├── RecordAcquire / RecordRelease     │
│  ├── goroutines map │  │  ├── RecordWait(id, dur, waiters)      │
│  ├── eventCh (256)  │  │  ├── RecordTimeout                     │
│  ├── eventWriter()  │  │  └── GetHistogram / GetAllMetrics      │
│  ├── broadcastUpdates│ │                                        │
│  │   (100 ms tick)  │  │  PrimitiveMetrics: atomic counters     │
│  └── updateChan(100)│  │  Histograms: 9 buckets 100µs–1s       │
└──────────┬──────────┘  └────────────────────────────────────────┘
           │
┌──────────▼──────────────────────────────────────────────────────┐
│                   PRIMITIVES LAYER  (internal/primitives/)      │
│                                                                 │
│  WaiterQueue (lock-free MPMC, CAS-linked nodes)                 │
│  Waiter      (channel-based park/unpark with cancellation)      │
│                                                                 │
│  RWLock      — writer-preference, atomic state word            │
│  Semaphore   — counting, AcquireN / ReleaseN                   │
│  Mutex       — non-handoff, lost-wakeup safe                   │
│  CondVar     — Wait / WaitTimeout / WaitFor / Broadcast        │
│  Barrier     — cyclic, generation-based, Break/Reset           │
│  WaitGroup   — context-cancellable Wait                        │
│  Once        — resettable (unlike sync.Once)                   │
│  Singleflight — deduplicating Do / DoChan / Forget             │
└─────────────────────────────────────────────────────────────────┘
```

---

## Features

### Eight Synchronization Primitives
| Primitive | Key API | Notes |
|---|---|---|
| **RWLock** | `RLock/RUnlock`, `Lock/Unlock`, `TryRLock`, `TryLock`, `RLockContext`, `LockContext` | Writer-preference; atomic `Int32` state word |
| **Semaphore** | `Acquire/Release`, `AcquireN/ReleaseN`, `TryAcquire`, `AcquireTimeout`, `AcquireContext` | Capacity-safe `Release` returns error on overflow |
| **Mutex** | `Lock/Unlock`, `TryLock`, `LockContext` | Non-handoff model; `sync.Locker` compliant |
| **CondVar** | `Wait(m)`, `WaitTimeout`, `WaitFor(m, cond)`, `Signal`, `Broadcast` | Spurious-wakeup-safe `WaitFor` helper |
| **Barrier** | `Wait`, `WaitTimeout`, `WaitContext`, `Break`, `Reset` | Cyclic; `ErrBarrierBroken` sentinel |
| **WaitGroup** | `Add`, `Done`, `Wait`, `WaitContext` | Panics on negative counter |
| **Once** | `Do(f)`, `Reset`, `Done` | Resettable; unlike `sync.Once` |
| **Singleflight** | `Do(key, fn)`, `DoChan`, `Forget` | `Shared` flag in `Result` |

### Real-Time WebSocket Dashboard
- Live canvas rendering of goroutine state transitions
- Per-primitive statistics panels
- Toast notifications for server errors
- Exponential-backoff reconnection (1 s → 30 s cap)
- XSS-safe: all user-supplied strings rendered as text nodes, never `innerHTML`

### Prometheus Metrics
- `/metrics` endpoint in Prometheus text exposition format (no external Prometheus client dependency)
- Per-primitive counters: `syncprim_acquires_total`, `syncprim_releases_total`, `syncprim_waits_total`, `syncprim_timeouts_total`
- Wait-duration histograms with 9 buckets: 100 µs, 500 µs, 1 ms, 5 ms, 10 ms, 50 ms, 100 ms, 500 ms, 1 s
- Global: `syncprim_contention_total`, `syncprim_uptime_seconds`, `syncprim_dropped_broadcasts_total`

### Snapshot Persistence
- Optional JSON file persistence via `Config.SnapshotPath`
- State saved on graceful shutdown; loaded on startup
- Per-connection isolation: each WebSocket connection manages its own primitive namespace

### Security
- TLS via `Config.TLSCertFile` / `Config.TLSKeyFile`
- Origin allowlist via `Config.AllowedOrigins` (empty = localhost only, `["*"]` = all)
- API key authentication via `Authorization: Bearer <key>` header
- Per-connection sliding-window rate limit: 200 messages/second
- Connection cap: `Config.MaxConns` (default 1000)
- HTTP server timeouts: ReadHeader=5 s, Read=10 s, Write=30 s, Idle=120 s
- WebSocket message size cap: 64 KiB per message

### Operations & Reliability
- Structured JSON logging via `log/slog`; enable with `LOG_FORMAT=json`
- Ping/pong keepalive: 30 s interval, 60 s pong timeout
- Graceful shutdown with 10 s drain window
- `dropped_broadcasts` counter exposed in `/healthz` and `/metrics`
- Multi-stage Docker image, non-root Alpine user
- GitHub Actions CI: build, vet, race detector, coverage gate (≥70%), benchmarks

---

## Quick Start

```bash
git clone https://github.com/sanskarpan/advanced-synchronization-primitives.git
cd advanced-synchronization-primitives
go run ./cmd/server/...
```

Open [http://localhost:8085](http://localhost:8085) in your browser.

The dashboard lets you create primitives, perform operations, and watch goroutine state transitions in real time.

---

## Installation

```bash
go get github.com/sanskar/syncprimitives
```

**Requirements:** Go 1.21+

---

## Usage Examples

### RWLock

```go
rw := primitives.NewRWLock()

// Concurrent reads
go func() {
    rw.RLock()
    defer rw.RUnlock()
    // read shared data
}()

// Exclusive write with context cancellation
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()
if err := rw.LockContext(ctx); err != nil {
    log.Printf("write lock timed out: %v", err)
    return
}
defer rw.Unlock()
// write shared data

// Non-blocking attempt
if rw.TryLock() {
    defer rw.Unlock()
    // write
}

stats := rw.GetStats()
fmt.Printf("reader acquires: %d, writer acquires: %d\n",
    stats.ReaderAcquires, stats.WriterAcquires)
```

### Semaphore

```go
// Limit to 5 concurrent workers
sem := primitives.NewSemaphore(5)

for i := 0; i < 20; i++ {
    go func(id int) {
        sem.Acquire()
        defer sem.Release()
        doWork(id)
    }(i)
}

// Bulk acquire with timeout
if !sem.AcquireNTimeout(3, 500*time.Millisecond) {
    log.Println("could not acquire 3 slots within 500ms")
}
```

### Mutex

```go
mu := primitives.NewMutex()
mu.Lock()
defer mu.Unlock()

// With context
ctx, cancel := context.WithTimeout(context.Background(), time.Second)
defer cancel()
if err := mu.LockContext(ctx); err != nil {
    return fmt.Errorf("lock acquisition failed: %w", err)
}
defer mu.Unlock()
```

### CondVar

```go
mu  := primitives.NewMutex()
cv  := primitives.NewCondVar()
ready := false

// Producer
go func() {
    mu.Lock()
    ready = true
    cv.Signal()
    mu.Unlock()
}()

// Consumer — use WaitFor for automatic spurious-wakeup safety
mu.Lock()
cv.WaitFor(mu, func() bool { return ready })
mu.Unlock()
```

### Barrier

```go
b := primitives.NewBarrier(4) // 4 goroutines must arrive

for i := 0; i < 4; i++ {
    go func(id int) {
        doPhase1(id)
        idx, err := b.Wait()
        if err != nil {
            log.Printf("barrier broken: %v", err)
            return
        }
        doPhase2(id, idx)
    }(i)
}
```

### WaitGroup

```go
wg := primitives.NewWaitGroup()
for i := 0; i < 10; i++ {
    wg.Add(1)
    go func() {
        defer wg.Done()
        doWork()
    }()
}

// With context
ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()
if err := wg.WaitContext(ctx); err != nil {
    log.Printf("WaitGroup timed out: %v", err)
}
```

### Once

```go
o := primitives.NewOnce()

for i := 0; i < 100; i++ {
    go func() {
        o.Do(func() {
            initializeDatabase() // called exactly once
        })
    }()
}

// Reset for testing or re-initialization
o.Reset()
o.Do(func() { reinitializeDatabase() })
```

### Singleflight

```go
g := primitives.NewGroup()

// 100 goroutines requesting the same key — only 1 network call made
for i := 0; i < 100; i++ {
    go func() {
        result, err := g.Do("cache-key", func() (interface{}, error) {
            return fetchFromRemote()
        })
        if result.Shared {
            // This goroutine shared another goroutine's result
        }
    }()
}

// Async variant
ch := g.DoChan("key", fetchFn)
result := <-ch
```

---

## HTTP Endpoints

| Method | Path | Description | Auth Required |
|--------|------|-------------|---------------|
| `GET` | `/ws` | WebSocket upgrade; all primitive operations | Yes (if APIKey set) |
| `GET` | `/` | Embedded HTML dashboard SPA | No |
| `GET` | `/healthz` | Health check: uptime, dropped broadcasts | No |
| `GET` | `/readyz` | Readiness probe; returns 200 when server is accepting traffic | No |
| `GET` | `/metrics` | Prometheus text format metrics | No |

### Health Check Response (`/healthz`)

```json
{
  "status": "ok",
  "uptime": "3h24m10s",
  "dropped_broadcasts": 0
}
```

### Metrics Endpoint (`/metrics`)

```
# HELP syncprim_acquires_total Total acquire operations per primitive
# TYPE syncprim_acquires_total counter
syncprim_acquires_total{id="mutex-1",type="Mutex",name="db-lock"} 4201

# HELP syncprim_wait_duration_seconds Wait duration histogram
# TYPE syncprim_wait_duration_seconds histogram
syncprim_wait_duration_seconds_bucket{id="mutex-1",le="0.001"} 3980
...
```

---

## WebSocket Message Protocol

All WebSocket messages are JSON objects with a `type` string field and a `payload` object.

### Inbound Messages (Client → Server)

| `type` | Required `payload` fields | Description |
|--------|--------------------------|-------------|
| `createRWLock` | `id` (string), `name` (string) | Create a new RWLock |
| `createSemaphore` | `id`, `name`, `capacity` (int32 ≥1) | Create a Semaphore |
| `createMutex` | `id`, `name` | Create a Mutex |
| `createCondVar` | `id`, `name` | Create a CondVar |
| `createBarrier` | `id`, `name`, `parties` (int32 ≥1) | Create a Barrier |
| `createWaitGroup` | `id`, `name` | Create a WaitGroup |
| `createOnce` | `id`, `name` | Create a Once |
| `createSingleflight` | `id`, `name` | Create a Singleflight Group |
| `primitiveOp` | `id`, `op` (string), `holdMs` (int, 1–5000) | Execute an operation on an existing primitive |
| `deletePrimitive` | `id` | Delete a primitive |

#### `primitiveOp` Operations by Type

| Primitive | `op` values |
|-----------|------------|
| RWLock | `rlock`, `runlock`, `lock`, `unlock`, `tryRLock`, `tryLock` |
| Semaphore | `acquire`, `release` |
| Mutex | `lock`, `unlock`, `tryLock` |
| CondVar | `wait`, `signal`, `broadcast` |
| Barrier | `wait`, `break`, `reset` |
| WaitGroup | `add`, `done`, `wait` |
| Once | `do`, `reset` |
| Singleflight | `do`, `forget` |

### Outbound Messages (Server → Client)

| `type` | `payload` contents | Description |
|--------|--------------------|-------------|
| `state` | Full primitives map | Sent immediately on connection; full state snapshot |
| `update` | Full primitives map | Periodic state broadcast (100 ms interval) |
| `success` | `{id, op, message}` | Operation completed successfully |
| `error` | `{message}` | Operation failed; displayed as toast in dashboard |
| `primitiveDeleted` | `{id}` | Primitive was deleted |

---

## Configuration

`web.Config` struct — passed to `web.NewServerWithConfig(cfg)`:

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `AllowedOrigins` | `[]string` | `[]` (localhost only) | WebSocket allowed origins. `["*"]` permits all origins. |
| `TLSCertFile` | `string` | `""` | Path to TLS certificate PEM file. Both cert and key must be set to enable TLS. |
| `TLSKeyFile` | `string` | `""` | Path to TLS private key PEM file. |
| `APIKey` | `string` | `""` | When non-empty, clients must send `Authorization: Bearer <key>`. |
| `MaxConns` | `int` | `1000` | Maximum simultaneous WebSocket connections. |
| `SnapshotPath` | `string` | `""` | File path for JSON state persistence. Empty disables persistence. |

### CLI Flags (`cmd/server`)

```
-addr           string   HTTP listen address (default ":8085")
-allowed-origins string  Comma-separated allowed WebSocket origins
-tls-cert        string  TLS certificate file path
-tls-key         string  TLS key file path
-api-key         string  WebSocket API key (empty = no auth)
```

### Environment Variables

| Variable | Description |
|----------|-------------|
| `LOG_FORMAT=json` | Enable JSON structured log output (default: text) |

---

## Package Layout

```
github.com/sanskar/syncprimitives
├── cmd/
│   └── server/
│       └── main.go          — entry point, flag parsing, signal handling
├── internal/
│   ├── primitives/          — all 8 primitives + WaiterQueue + Waiter
│   │   ├── waiter.go        — Waiter (channel park/unpark) + WaiterQueue (lock-free)
│   │   ├── rwlock.go        — RWLock
│   │   ├── semaphore.go     — Semaphore
│   │   ├── condvar.go       — Mutex + CondVar (same file; CondVar depends on Mutex)
│   │   ├── barrier.go       — Barrier
│   │   ├── waitgroup.go     — WaitGroup
│   │   ├── once.go          — Once
│   │   └── singleflight.go  — Singleflight Group
│   ├── metrics/
│   │   └── metrics.go       — MetricsCollector, PrimitiveMetrics, histograms
│   ├── scheduler/
│   │   └── scheduler.go     — Scheduler (primitive registry, goroutine tracking, events)
│   └── loadtest/
│       └── loadtest_test.go — 50-connection load test + WebSocket benchmark
├── web/
│   ├── server.go            — HTTP server, WebSocket handler, snapshot, Prometheus
│   ├── server_test.go       — 11 integration tests
│   └── static/
│       ├── index.html       — dashboard SPA
│       ├── css/             — stylesheet
│       └── js/              — WebSocket client, canvas renderer
├── examples/
│   ├── rwlock/
│   ├── semaphore/
│   ├── barrier/
│   └── condvar/
├── Dockerfile               — multi-stage, non-root Alpine
├── Makefile                 — build / test / race / bench / coverage / lint
└── .github/workflows/ci.yml — GitHub Actions CI
```

---

## Benchmarks

Run with `make bench` or `go test -bench=. -benchmem ./internal/primitives/`.

| Benchmark | Operations | ns/op | B/op | allocs/op |
|-----------|-----------|-------|------|-----------|
| `BenchmarkMutexUncontended` | Uncontended lock/unlock | ~25 | 0 | 0 |
| `BenchmarkMutexContended` | 8-goroutine contention | ~180 | 0 | 0 |
| `BenchmarkRWLockRead` | Read-only, no writers | ~30 | 0 | 0 |
| `BenchmarkRWLockReadHeavy` | 7 readers / 1 writer | ~95 | 0 | 0 |
| `BenchmarkSemaphore` | Acquire/Release cycle | ~45 | 0 | 0 |
| `BenchmarkSemaphoreBurst` | 10-goroutine burst | ~210 | 0 | 0 |
| `BenchmarkBarrierSmall` | 2-party barrier | ~310 | 48 | 2 |
| `BenchmarkBarrierLarge` | 16-party barrier | ~1,100 | 48 | 2 |

*Numbers are representative on an Apple M-series CPU. Run your own benchmarks on target hardware.*

WebSocket throughput (8 workers, single connection per worker):

| Benchmark | ops/sec |
|-----------|---------|
| `BenchmarkWebSocketConcurrentOps` | ~850 create/lock/delete cycles/s per worker |

---

## Running the Server

```bash
# Development (plaintext, no auth)
go run ./cmd/server/... -addr :8085

# With TLS
go run ./cmd/server/... -addr :8443 -tls-cert server.crt -tls-key server.key

# With API key
go run ./cmd/server/... -api-key my-secret-key

# Docker
docker build -t syncprimitives .
docker run -p 8085:8085 syncprimitives

# JSON logs
LOG_FORMAT=json go run ./cmd/server/...
```

---

## Testing

```bash
make test       # standard tests
make race       # tests with race detector (runs 3 times)
make bench      # benchmarks
make coverage   # coverage report, enforces ≥70% gate
```

---

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for setup instructions, branch naming, commit format, PR requirements, and the security reporting process.

---

## License

MIT License — see [LICENSE](LICENSE).

Copyright (c) 2026 Sanskar Pandey
