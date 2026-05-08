# Architecture Improvements

**Project:** Advanced Synchronization Primitives
**Document Type:** Architectural Analysis and Improvement Proposals
**Date:** 2026-05-09

This document analyzes the current architecture, identifies design weaknesses at each layer, proposes concrete improvements, outlines migration paths, and discusses trade-offs.

---

## Current Architecture

### Package and Data Flow Diagram

```
┌─────────────────────────────────────────────────────────────────────────┐
│                          PROCESS BOUNDARY                               │
│                                                                         │
│  cmd/server/main.go                                                     │
│  ├── flag.Parse() → web.Config                                          │
│  ├── web.NewServerWithConfig(cfg) → *web.Server                        │
│  ├── server.Start(addr) [goroutine]                                     │
│  └── server.Shutdown(ctx) [on SIGINT/SIGTERM]                          │
│                                                                         │
│  web/server.go  (*web.Server)                                           │
│  │                                                                      │
│  │  HTTP multiplexer (net/http.ServeMux)                                │
│  │  ├── GET /            → HandleStatic (embedded staticFS)            │
│  │  ├── GET /ws          → HandleWebSocket                             │
│  │  ├── GET /healthz     → HandleHealthz                               │
│  │  ├── GET /readyz      → HandleReadyz                                │
│  │  └── GET /metrics     → HandleMetrics                               │
│  │                                                                      │
│  │  Per-Server State:                                                   │
│  │  ├── connCount    atomic.Int32   (active WebSocket connections)     │
│  │  ├── droppedBcasts atomic.Int64  (scheduler update drops)          │
│  │  ├── scheduler    *scheduler.Scheduler                              │
│  │  └── metrics      *metrics.MetricsCollector                        │
│  │                                                                      │
│  │  HandleWebSocket (per connection)                                    │
│  │  ├── Auth: Bearer header check                                      │
│  │  ├── Origin check (Config.AllowedOrigins)                          │
│  │  ├── connCount check (Config.MaxConns)                             │
│  │  ├── per-conn state: {primitives map, connState{msgTimes}}          │
│  │  ├── Send initial state snapshot                                    │
│  │  ├── readMessages() [goroutine: reads WS frames]                   │
│  │  ├── forwardSchedulerUpdates() [goroutine: broadcasts]             │
│  │  └── pingLoop() [goroutine: keepalive]                             │
│  │                                                                      │
│  │  Per-Connection Primitive Map                                        │
│  │  map[string]primEntry{ctx, cancel}                                  │
│  │  Each entry holds a reference to one of:                            │
│  │  *primitives.RWLock, *Semaphore, *Mutex, *CondVar,                 │
│  │  *Barrier, *WaitGroup, *Once, *Group                               │
│  │                                                                      │
│  internal/scheduler/scheduler.go  (*Scheduler)                         │
│  ├── primitives  map[string]*PrimitiveInfo  (all connections, shared)  │
│  ├── goroutines  map[uint64]*GoroutineInfo                             │
│  ├── events      []Event (ring buffer, cap 1000)                       │
│  ├── eventCh     chan Event (256 buffer)                               │
│  ├── eventWriter [goroutine: consumes eventCh → events]               │
│  ├── broadcastUpdates [goroutine: 100ms tick → updateChan]            │
│  └── updateChan  chan *SchedulerUpdate (100 buffer)                    │
│                                                                         │
│  internal/metrics/metrics.go  (*MetricsCollector)                      │
│  ├── primitiveMetrics  map[string]*PrimitiveMetrics                    │
│  ├── Per-primitive: atomic counters, histogram buckets                 │
│  └── sync.RWMutex for map access                                       │
│                                                                         │
│  internal/primitives/                                                   │
│  ├── WaiterQueue  (lock-free MPMC linked queue)                        │
│  ├── Waiter       (channel-based park/unpark + cancelled flag)         │
│  └── [8 primitives]                                                    │
│                                                                         │
└─────────────────────────────────────────────────────────────────────────┘
```

### Key Architectural Decisions in v0.1.0

1. **Per-connection isolation**: Each WebSocket connection owns its own map of primitives. No two connections share primitive instances. This is safe and simple but prevents collaborative use cases.

2. **Scheduler is a shared singleton**: The `Scheduler` tracks primitives across all connections in a single shared map. This is useful for cross-connection observability but creates lock contention under load.

3. **Metrics collector is a shared singleton**: `MetricsCollector` tracks metrics across all connections. Same trade-off as the Scheduler.

4. **Non-handoff Mutex unlock**: The Mutex stores `0` before waking a waiter. The woken goroutine must re-CAS. This avoids a class of bugs where a cancelled waiter is dequeued and the lock appears acquired with no owner.

5. **Writer-preference RWLock**: New readers queue behind waiting writers. Prevents writer starvation at the cost of potential reader starvation under heavy write load.

6. **Event decoupling via `eventCh`**: The Scheduler emits events into a buffered channel rather than writing to the events slice directly while holding `mu`. This prevents nested-lock deadlocks between `mu` and `eventsMu`.

---

## Identified Design Weaknesses

### Layer 1: Primitives

**W1.1 — Writer-preference RWLock starvation under sustained write load**
The current `RWLock` implementation can permanently starve readers when writers arrive faster than they are served. The `writerQueue.IsEmpty()` check in `RLock` blocks all new readers as soon as one writer enqueues. If writers are continuous, readers never acquire. This is documented in the code but has no mitigation.

**W1.2 — Waiter allocation on every blocking call**
`NewWaiter()` always allocates a new struct and a buffered channel (`make(chan struct{}, 1)`). Under high contention, this creates significant GC pressure. The channel itself is never garbage-collected until the struct is, even if the waiter is cancelled and the channel is never read.

**W1.3 — `Barrier` over-subscription returns incorrect error type**
`Barrier.WaitContext` returns `context.DeadlineExceeded` for an over-subscription event (arrived > parties). This is semantically wrong. Over-subscription is a programming error, not a deadline exceedance. It should return a distinct error type.

**W1.4 — `Singleflight.Do` does not propagate panics from `fn`**
If `fn()` panics, the panic is caught by the Go runtime but not the `wg.Done()` inside `Do`. All other goroutines waiting on `c.wg.Wait()` will block forever. The standard library `golang.org/x/sync/singleflight` includes panic propagation. Our implementation does not.

**W1.5 — `Once.Reset` race with concurrent `Do`**
`Reset` acquires `mu` and sets `done=0`. If a goroutine is currently inside `f()` (between the `o.doCalls.Add(1)` and the deferred `o.done.Store(1)`), a concurrent `Reset` followed by another `Do` can cause `f` to be called a second time before the first call completes. This violates the single-execution contract and can cause initialization logic to run twice.

**W1.6 — No cancellation support in `CondVar.Wait`**
`CondVar.Wait(m)` blocks indefinitely. There is no `WaitContext` variant for the raw wait (only `WaitTimeout`). Callers who need timeout-based waiting with a context must use `WaitTimeout` with a manually computed deadline, losing access to context cancellation semantics.

### Layer 2: Metrics

**W2.1 — Hardcoded histogram bucket boundaries**
The 9-bucket histogram (`[100µs, 500µs, 1ms, 5ms, 10ms, 50ms, 100ms, 500ms, 1s]`) is hardcoded. Applications with sub-microsecond or multi-second wait times cannot observe their distribution accurately.

**W2.2 — No Prometheus client library validation**
The `/metrics` endpoint manually constructs Prometheus text format. It is not validated against `prometheus/common/expfmt`. A format regression would silently cause Prometheus to drop all metrics.

**W2.3 — `GlobalMetrics` includes no per-type breakdown**
The global metrics aggregate all primitive types together. Operators cannot distinguish whether the 10,000 acquisitions came from mutexes, semaphores, or rwlocks without inspecting per-primitive metrics.

**W2.4 — Min/max wait time uses CAS loop without atomic min/max helper**
The min/max tracking in `RecordWait` uses explicit CAS loops. This is correct but verbose. Go 1.23 introduces `atomic.Int64.CompareAndSwap` improvements; a helper function would reduce duplication.

### Layer 3: Scheduler

**W3.1 — Shared Scheduler across all connections**
The Scheduler is a single shared instance. All connections share the same primitive and goroutine maps. Under high connection load, `mu.Lock()` becomes a bottleneck because every goroutine block/unblock event (which happens on every primitive operation) acquires the global lock.

**W3.2 — goroutine map uses `uint64` key with no external assignment**
Goroutine IDs are assigned by the caller and have no coordination. Two different connections could register goroutines with the same ID, silently overwriting each other's entries in the shared map.

**W3.3 — Broadcast tick is fixed at 100 ms**
The 100 ms broadcast interval is hardcoded. High-frequency scenarios need faster updates. Low-frequency monitoring scenarios waste bandwidth with 10 updates/second when state changes once per minute.

**W3.4 — Event ring buffer uses slice append with linear shift**
The events ring buffer shifts the slice when it exceeds `maxEvents`:
```go
s.events = s.events[len(s.events)-s.maxEvents:]
```
This allocates a new backing array every time the ring overflows. A fixed-capacity circular buffer (pre-allocated `[1000]Event` array with head/tail indices) would eliminate these allocations.

### Layer 4: Server and API

**W4.1 — No input validation layer**
All message types are deserialized from JSON and dispatched without validating string lengths, integer ranges, or type constraints. A single oversized `id` string is stored in the scheduler and broadcast to all clients.

**W4.2 — API key accepted via URL query parameter**
The `?key=<apikey>` authentication path leaks credentials into server logs, reverse proxy logs, and browser history.

**W4.3 — Per-connection primitive map uses plain `map` under message-read lock**
The per-connection primitive map (`map[string]primEntry`) is accessed under the same mutex as message reading. Long-running operations (blocked `Acquire`) hold the context lock, potentially delaying new message processing for that connection.

**W4.4 — Snapshot persistence writes the entire state atomically**
The snapshot is written with `os.WriteFile`, which replaces the file atomically on most platforms. However, there is no fsync after the write, meaning the file can be lost on a power failure before the OS flushes the page cache.

**W4.5 — No request ID for correlating WebSocket operations**
`primitiveOp` responses do not include a request ID that the client sent. If a client sends multiple concurrent operations, it cannot correlate a `success` response to a specific request.

**W4.6 — `holdMs` clamp silently modifies client-requested value**
When a client sends `holdMs: 10000` (exceeding the current 5000 ms cap), the server silently clamps it to 5000 ms without notifying the client. The client has no way to know its requested hold duration was modified.

### Layer 5: Frontend

**W5.1 — Canvas rendering is not rate-limited**
The frontend re-renders the canvas on every WebSocket `update` message (every 100 ms). If the client is slow or has many primitives, this can cause frame drops and make the browser tab CPU-intensive.

**W5.2 — No reconnection state indicator**
The exponential-backoff reconnection shows a "Reconnecting…" message but does not indicate how many reconnection attempts have been made or how long until the next attempt.

**W5.3 — Primitive controls have no optimistic UI updates**
When a user clicks "Lock," the button becomes disabled but the UI does not update to show the primitive as locked until the next server `update` message arrives (up to 100 ms later). This creates perceived latency.

**W5.4 — No handling for server-side rate limit rejection**
When the server rejects a message due to the 200 msg/s rate limit, it returns a `{"type":"error","payload":{"message":"rate limit exceeded"}}`. The frontend shows this as a generic toast but does not throttle subsequent sends.

### Layer 6: Infrastructure

**W6.1 — No Kubernetes readiness probe validation**
The `/readyz` endpoint always returns 200. It does not check whether the scheduler's `broadcastUpdates` goroutine is alive, whether the event channel is full, or whether the server is actually accepting WebSocket connections.

**W6.2 — Docker image uses `alpine:3.19` without explicit digest**
Using a floating tag like `alpine:3.19` means the image content can change between builds if Alpine releases a patch. For reproducible builds, images should be pinned to their SHA256 digest.

**W6.3 — No resource limits in Dockerfile or example manifests**
The Docker image has no default resource constraints (CPU, memory). Running the server without limits allows it to consume all available memory under malicious input.

---

## Proposed Improvements per Layer

### Primitives Layer

**P1.1 — Add `FairRWLock` and `ReaderPrefRWLock` variants**
Provide a selection of RWLock fairness policies. Users choose the variant that matches their workload characteristics.

Migration: These are new types. Existing `RWLock` users are unaffected.

Trade-off: `FairRWLock` has higher overhead per operation (single queue traversal on every release vs. dual queue). `ReaderPrefRWLock` has potential writer starvation, which is the symmetric problem to the current implementation.

**P1.2 — `sync.Pool` for Waiter allocation**
```go
var waiterPool = sync.Pool{
    New: func() interface{} {
        return &Waiter{ch: make(chan struct{}, 1)}
    },
}
```
All `NewWaiter()` calls use `waiterPool.Get()`. All dequeue points call a `releaseWaiter(w)` function that zeroes the waiter's fields and returns it to the pool.

Migration: Internal refactor. No API change.

Trade-off: Pool objects are not garbage-collected under GC pressure in all cases. If a waiter is accidentally returned to the pool while still being waited on, a future acquisition will receive a pre-signaled waiter, causing an immediate spurious wakeup. This requires careful discipline in all dequeue paths.

**P1.3 — Fix `Singleflight.Do` panic propagation**
Follow the pattern from `golang.org/x/sync/singleflight`:
```go
defer func() {
    if !normalReturn && !recovered {
        c.err = newPanicError(r)
    }
    c.wg.Done()
    // ... delete from map
}()
```
Propagate the panic to all waiting callers.

Migration: Behavioral change. Callers that previously blocked forever on a panicking `fn` will now panic themselves. This is the correct behavior. Add to CHANGELOG as a fix.

**P1.4 — Add `CondVar.WaitContext(m *Mutex, ctx context.Context) error`**
A missing context-aware variant of `Wait`. Implementation is analogous to `WaitTimeout` but uses `waiter.WaitContext(ctx)` instead of `waiter.WaitTimeout(timeout)`.

Migration: Purely additive. No breaking changes.

### Metrics Layer

**P2.1 — Make histogram buckets configurable via `web.Config`**
Add `HistogramBuckets []time.Duration` to `Config`. Thread it through `NewServerWithConfig` → `metrics.NewMetricsCollectorWithBuckets(buckets)`.

Migration: Additive field in `Config`. Zero value uses the current hardcoded defaults.

**P2.2 — Add Prometheus client library format validation test**
Add `TestMetricsPrometheusFormat` in `web/server_test.go`:
```go
import "github.com/prometheus/common/expfmt"
// ... parse the /metrics response with expfmt.TextToMetricFamilies
// ... assert expected metric families exist
```

Migration: Test-only change. Adds a test dependency on `github.com/prometheus/common`.

**P2.3 — Add per-type metric breakdown in GlobalMetrics**
Add `ByType map[string]TypeMetrics` to `GlobalMetrics`. Populate it in `GetGlobalMetrics()` by iterating over `primitiveMetrics` and grouping by `PrimitiveMetrics.Type`.

Migration: Additive field. No breaking changes.

### Scheduler Layer

**P3.1 — Per-connection scheduler isolation (long-term)**
Instead of one shared `Scheduler`, each WebSocket connection creates its own `*scheduler.Scheduler`. The server aggregates metrics from all per-connection schedulers for the `/metrics` endpoint via a registry.

Migration: Major refactor. Requires a `SchedulerRegistry` type and changes to how the server reads metrics. Phased: first make `Scheduler.GetAllMetrics()` thread-safe under concurrent modification, then add per-connection instances.

Trade-off: Eliminates global lock contention on the scheduler. Complicates aggregated observability (metrics must be summed across all instances). Memory usage scales linearly with connection count.

**P3.2 — Fixed-capacity circular ring buffer for events**
Replace the `[]Event` slice with a pre-allocated `[1000]Event` array and `head`, `tail`, `count` indices.

Migration: Internal refactor. No API change. `GetEvents(limit)` behavior is preserved.

**P3.3 — Configurable broadcast interval**
Add `BroadcastInterval time.Duration` to `Config`. Default to 100 ms.

Migration: Additive config field.

**P3.4 — Globally unique goroutine ID assignment**
Replace caller-provided goroutine IDs with server-assigned UUIDs generated in `RegisterGoroutine`. The caller provides a `name` string; the server returns the assigned ID.

Migration: API change to `RegisterGoroutine`. Requires callers to store the returned ID for later `BlockGoroutine`/`UnregisterGoroutine` calls. Breaking change; coordinate with any external integrations.

### Server and API Layer

**P4.1 — Centralized validation middleware**
Extract validation into a `validateMessage(msg *inboundMessage) error` function. Validate: `id` max 256 chars, `name` max 256 chars, `holdMs` in [1, 3600000], `capacity` > 0, `parties` > 0. Return structured error responses with field names.

Migration: Internal refactor. No breaking API changes for well-formed requests.

**P4.2 — Remove URL query parameter API key**
Delete the `r.URL.Query().Get("key")` check. This is a breaking change but is required for security.

Migration: Announce in CHANGELOG with a migration guide. Clients using `?key=` must update to `Authorization: Bearer <key>`.

**P4.3 — Request correlation IDs**
Add an optional `reqID` field to all inbound messages. Echo it in all outbound success/error responses. Clients that send `reqID` can correlate async responses.

Migration: Additive protocol change. Clients that do not send `reqID` see no change (the field is omitted from responses).

**P4.4 — fsync snapshot writes**
After `os.WriteFile`, open the file and call `f.Sync()` to ensure the data is flushed to disk before the OS page cache can lose it.

Migration: Internal change. No API impact.

**P4.5 — Notify client when `holdMs` is clamped**
When the requested `holdMs` exceeds the configured maximum, include a `warning` field in the `success` response: `{"type":"success","payload":{"id":"x","op":"lock","holdMs":3600000,"warning":"holdMs clamped from 7200000 to 3600000"}}`.

Migration: Additive response field. Existing clients that ignore unknown fields are unaffected.

### Frontend Layer

**P5.1 — Rate-limit canvas re-render with `requestAnimationFrame`**
Replace the on-message canvas re-render with `requestAnimationFrame`. Accumulate updates and render at the display refresh rate (60 fps) rather than on every WebSocket message.

Migration: JavaScript-only change. No server changes.

**P5.2 — Client-side rate limit backoff**
When the server returns a `rate limit exceeded` error, implement client-side exponential backoff for outbound messages: multiply the send interval by 2 (up to 5 s) after each rejection, then gradually reduce back to normal.

Migration: JavaScript-only change.

**P5.3 — Optimistic UI updates**
When a lock operation is initiated, immediately update the UI to show the primitive in a "pending" state (e.g., orange instead of green). Revert to the server-confirmed state when the `success` or `error` response arrives.

Migration: JavaScript-only change. No server changes.

### Infrastructure Layer

**P6.1 — Pin Docker base image to SHA256 digest**
Change `FROM alpine:3.19` to `FROM alpine@sha256:<verified-digest>`. Pin the Go builder image similarly.

Migration: Update Dockerfile. Check and update the digest periodically with Dependabot or Renovate.

**P6.2 — Add meaningful `/readyz` checks**
Return 503 from `/readyz` if:
- The scheduler's `running` flag is false
- The event channel is 90%+ full (`len(eventCh) > 230`)
- No broadcast has been sent in the last 5 s

Migration: Changes the semantics of `/readyz`. Kubernetes readiness probes will temporarily mark the pod unready if the event channel is saturated, which is the correct behavior.

**P6.3 — Add resource constraints to Dockerfile**
Document recommended resource limits in the Dockerfile comments and Kubernetes manifests: `512Mi` memory limit, `0.5` CPU request.

---

## Migration Paths

### Migration 1: Validating Existing Clients Against New Input Limits

**Impact:** Any client sending `id` or `name` longer than 256 characters will receive an error response instead of success.

**Steps:**
1. Deploy the updated server.
2. Monitor `/metrics` for `syncprim_validation_errors_total` (new counter) to identify clients sending oversized fields.
3. Update affected clients to use shorter IDs.
4. No rollback needed — old clients sending valid fields are unaffected.

### Migration 2: API Key URL Parameter Removal

**Impact:** Clients using `?key=<apikey>` authentication will receive 401 Unauthorized.

**Steps:**
1. Add a deprecation warning in the server: log `slog.Warn("API key via query parameter is deprecated, use Authorization: Bearer header")` on every use of the old path. Deploy this version.
2. Monitor logs to identify clients still using the deprecated path.
3. After a 30-day deprecation period, remove the query parameter path entirely.
4. Rollback: revert to the version with both paths. No data is affected.

### Migration 3: Snapshot Format Versioning

**Impact:** Existing snapshot files without a version field will be rejected on startup.

**Steps:**
1. Implement read-compatibility: on startup, attempt to parse the snapshot as the new versioned format first. If that fails, fall back to attempting the old flat format and log a warning: `"Loaded snapshot in legacy format; will be written in versioned format on next shutdown."` Write the snapshot in the new format on the next clean shutdown.
2. After one release cycle, remove the legacy format reader.

### Migration 4: Per-Connection Scheduler Isolation (Long-Term)

**Impact:** Major internal refactor. No external API change.

**Steps:**
1. Phase A: Make `Scheduler` safe for concurrent use by multiple readers calling `GetMetrics()` and `GetAllMetrics()` without holding `mu`. (Already true due to `RLock` on read paths.)
2. Phase B: Introduce `SchedulerRegistry` that aggregates metrics across multiple `Scheduler` instances.
3. Phase C: Create one `Scheduler` per WebSocket connection. Aggregate for `/metrics`.
4. Phase D: Remove the global shared `Scheduler`.

Each phase is independently deployable and testable.

---

## Trade-Off Analysis

### Trade-off 1: Per-Connection vs. Shared Scheduler

| Factor | Current (Shared) | Proposed (Per-Connection) |
|--------|-----------------|--------------------------|
| Lock contention | High under 1000+ connections | Eliminated |
| Cross-connection visibility | Full (all primitives visible in one map) | Requires aggregation layer |
| Memory per connection | O(1) for scheduler | O(primitives) per connection |
| Implementation complexity | Simple | Medium |
| Recommended for | <100 concurrent connections | >100 concurrent connections |

Decision: Implement per-connection scheduler in Phase 3. The aggregation overhead is justified by the elimination of the global lock bottleneck.

### Trade-off 2: Writer-Preference vs. Fair RWLock

| Factor | Writer-Preference (current) | Fair (FIFO) |
|--------|----------------------------|-------------|
| Writer starvation | None | None |
| Reader starvation | Possible under write-heavy load | None |
| Throughput (read-heavy) | Lower (unnecessary reader queuing) | Higher |
| Throughput (write-heavy) | Higher (writers get priority) | Lower |
| Implementation complexity | Current | +40% LOC |
| Context-switch overhead | Low | Medium (single queue traversal) |

Decision: Provide both variants. Default remains writer-preference for backward compatibility. Fair variant added in Phase 3.

### Trade-off 3: Waiter Pool vs. Allocation

| Factor | Allocation (current) | sync.Pool |
|--------|---------------------|-----------|
| Memory pressure | High under contention | Minimal |
| GC pause frequency | Higher | Lower |
| Implementation risk | Low | Medium (must not return in-use waiters) |
| Benchmark impact | Baseline | -60% allocations in benchmarks |
| Channel creation | Per-operation | Once per pool object |

Decision: Implement in Phase 7. The implementation risk requires careful testing with the race detector.

### Trade-off 4: Delta Updates vs. Full State Broadcast

| Factor | Full Broadcast (current) | Delta Updates |
|--------|-------------------------|---------------|
| Server CPU (serialization) | O(primitives) per tick | O(changed_primitives) per tick |
| Network bandwidth | ~50 KB/tick at 100 primitives | ~5 KB/tick typical |
| Client complexity | Simple map replace | Requires merge logic |
| Missed update handling | Not needed | Requires sequence numbers + full refresh |
| Implementation complexity | Low | High |

Decision: Implement in Phase 7. The bandwidth savings justify the complexity only when primitive count exceeds ~50 per connection.

### Trade-off 5: JWT vs. Static API Key

| Factor | Static API Key (current) | JWT |
|--------|-------------------------|-----|
| Implementation complexity | Trivial | Medium (JWT validation, key rotation) |
| Expiry | Never | Configurable |
| Role encoding | None | Claims-based |
| Multi-tenant isolation | None | Namespace claim |
| Key rotation | Server restart | Issue new token |
| Security level | Low (single shared secret) | High |

Decision: Implement JWT in Phase 6. The static API key is appropriate for v0.1.0 but must be replaced before multi-tenant deployment.
