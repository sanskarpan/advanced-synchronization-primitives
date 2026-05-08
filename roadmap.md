# Product Roadmap

**Project:** Advanced Synchronization Primitives
**Module:** `github.com/sanskar/syncprimitives`
**Current Version:** v0.1.0
**Last Updated:** 2026-05-09

This roadmap is organized into seven phases across fourteen two-week sprints. Each item includes the problem it solves, the technical impact, estimated complexity (S = 1–2 days, M = 3–5 days, L = 1–2 weeks), and its dependencies.

---

## Phase 1 — Security and Stability Hardening (Sprint 1–2, ~2 weeks)

**Goal:** Eliminate known security gaps and correctness edge cases identified in the v0.1.0 audit before any public release.

---

### 1.1 Input Length Validation (ID/Name max 256 chars)

**Why it matters:** The server currently stores primitive IDs and names as map keys and in JSON responses with no length limit. A malicious or buggy client can send a 1 MB string as an ID, causing unbounded memory allocation in the scheduler's `primitives` map and in every JSON broadcast to all connected clients. Under the 200 msg/s rate limit, a single connection can still force 200 × 1 MB = 200 MB/s of allocation.

**Current limitation:** `createRWLock`, `createMutex`, and all other create handlers accept arbitrary-length `id` and `name` fields directly from JSON without validation.

**Business/technical impact:** Without this fix, a single malicious connection can degrade memory performance for all other connections sharing the same process.

**Implementation:** Validate in the JSON parsing layer in `web/server.go` immediately after `json.Unmarshal`. Return a `{"type":"error","payload":{"message":"id exceeds maximum length of 256 characters"}}` response and drop the message.

**Complexity:** S

**Dependencies:** None.

---

### 1.2 `holdMs` Upper Bound (max 3,600,000 ms = 1 hour)

**Why it matters:** `holdMs` controls how long the server holds a primitive lock on behalf of a client before auto-releasing. Currently clamped to [1, 5000] ms. However, a value of 5000 ms means a single `lock` operation can block an entire primitive for 5 seconds. Raising the ceiling to 1 hour (3,600,000 ms) while enforcing the upper bound prevents a client from creating effectively permanent locks that block all other users of that primitive.

**Current limitation:** The clamp is `[1, 5000]` ms, which is too restrictive for legitimate use cases (e.g., simulating a long-running database transaction) and was chosen arbitrarily.

**Business/technical impact:** The current limit forces users to chain multiple operations to simulate long holds, complicating client code. An explicit 1-hour ceiling prevents accidental or malicious indefinite holds.

**Implementation:** Change the clamp constants in the `primitiveOp` handler. Add a validation error when `holdMs > 3600000`.

**Complexity:** S

**Dependencies:** 1.1 (validation framework already in place).

---

### 1.3 API Key Security — Bearer Header Only, Remove URL Query Parameter

**Why it matters:** The current implementation accepts the API key via both `Authorization: Bearer <key>` and the `?key=<value>` URL query parameter. URL parameters are logged in web server access logs, reverse proxy logs, browser history, and are visible in network monitoring tools. This exposes the API key at rest in multiple places.

**Current limitation:** `web/server.go` `HandleWebSocket` checks both `r.Header.Get("Authorization")` and `r.URL.Query().Get("key")`.

**Business/technical impact:** Any operator running this behind nginx, Caddy, or a cloud load balancer will inadvertently log their API key in plain text. This is a credential leak waiting to happen.

**Breaking change:** Remove the `?key=` query parameter support entirely. Clients must migrate to `Authorization: Bearer <key>`. Document the migration in CHANGELOG.

**Complexity:** S

**Dependencies:** None.

---

### 1.4 WaitGroup Panic on Negative `Add`

**Why it matters:** The standard `sync.WaitGroup` panics when the counter goes negative because a negative counter indicates a programming error (calling `Done` more times than `Add`). Our implementation currently checks this in `Add` and `Done` with an `if n < 0` guard and panics — this is already implemented correctly. This item is about ensuring the panic message is clearly actionable.

**Current limitation:** The panic message is `"waitgroup: negative counter"`. Add context including the current counter value and the delta.

**Business/technical impact:** Better diagnostics reduce debugging time.

**Complexity:** S

**Dependencies:** None.

---

### 1.5 Snapshot Format Versioning

**Why it matters:** The JSON snapshot file written by `Config.SnapshotPath` has no version field. If the schema changes in a future release, the server will attempt to deserialize a v1 snapshot into a v2 struct, potentially failing silently or loading corrupt state.

**Current limitation:** Snapshot JSON is a flat `map[string]PrimitiveSnapshot`. There is no `{"version": 1, ...}` envelope.

**Business/technical impact:** Without versioning, any schema change to the snapshot format is a breaking, undetectable corruption. With versioning, the server can detect schema mismatch and either migrate or refuse to load the old snapshot.

**Implementation:** Wrap the snapshot in `{"version": 1, "primitives": {...}}`. On load, check the version field. If it differs from the current version constant, log a warning and skip loading (safe fallback to empty state).

**Complexity:** S

**Dependencies:** None.

---

### 1.6 `CondVar.WaitTimeout` Elapsed Time Tracking Fix

**Why it matters:** In `CondVar.WaitTimeout`, the `totalWaitTime` counter is only incremented when the waiter is signaled (`signaled == true`). When the wait times out, `totalWaitTime` is not updated even though time was spent waiting. This means `CondVarStats.TotalWaitTimeNs` is systematically undercounted when timeouts are common.

**Current limitation:** In `condvar.go`, the `else` branch (timeout path) sets `waiter.cancelled.Store(true)` but does not record `time.Since(startTime)` into `totalWaitTime`.

**Business/technical impact:** Prometheus metrics for CondVar average wait time will be inaccurate, leading to misleading dashboards and alerting thresholds.

**Implementation:** Record elapsed time unconditionally after `waiter.WaitTimeout` returns, regardless of whether it returned `true` or `false`.

**Complexity:** S

**Dependencies:** None.

---

### 1.7 `gosec` Static Analysis in CI

**Why it matters:** `gosec` performs security-focused static analysis: detecting hardcoded credentials, weak random number usage, SQL injection patterns, unsafe operations, and SSRF candidates. Running it in CI ensures security regressions are caught before merge.

**Current limitation:** CI runs only `go vet`, which does not check for security patterns.

**Business/technical impact:** Catches categories of issues that `go vet` does not: hardcoded secrets, unsafe pointer operations, integer overflow in security-sensitive code paths.

**Implementation:** Add a step to `.github/workflows/ci.yml`:
```yaml
- name: Security scan (gosec)
  run: |
    go install github.com/securego/gosec/v2/cmd/gosec@latest
    gosec -severity medium -confidence medium ./...
```

**Complexity:** S

**Dependencies:** None.

---

## Phase 2 — Observability and Reliability (Sprint 3–4, ~2 weeks)

**Goal:** Make the system production-observable, add resilience to operator mistakes, and eliminate remaining reliability gaps.

---

### 2.1 Structured JSON Logging Across All Packages

**Why it matters:** `web/server.go` uses `log/slog` correctly. However, `internal/primitives/` does not log at all, and `internal/scheduler/` uses `fmt.Println` in some paths. Consistent structured logging is required for centralized log aggregation (Datadog, Splunk, CloudWatch).

**Current limitation:** Primitives and scheduler have no logging. When a panic is recovered in `handlePrimitiveOp`, the stack trace is logged but without request context (connection ID, primitive ID).

**Business/technical impact:** Without structured logs, operators cannot correlate a client-reported error with server-side log entries. JSON logs are parseable by every log aggregation platform.

**Implementation:** Thread a `*slog.Logger` (or use `slog.Default()`) into the WebSocket handler's per-connection context. Log with `slog.Error` on all recovered panics and `slog.Warn` on rate-limit drops and dropped broadcasts, always including `connID` and `primitiveID` fields.

**Complexity:** M

**Dependencies:** 1.1 (connection IDs make logs more useful).

---

### 2.2 Operation-Level Rate Limiting (Per-Connection, Per-Second)

**Why it matters:** The current 200 msg/s limit is applied at the message-parsing level, before the message type is known. This means 200 `primitiveOp` messages (each spawning a goroutine and sleeping for `holdMs`) can be submitted per second per connection. With the 1-hour holdMs ceiling, this creates a goroutine accumulation risk.

**Current limitation:** Rate limiting in `connState` counts all messages equally. It does not distinguish between cheap operations (read stats) and expensive operations (acquire a lock for 1 hour).

**Business/technical impact:** Prevents a single connection from accumulating thousands of long-running goroutines that hold primitives indefinitely.

**Implementation:** Track `primitiveOp` messages separately with a tighter per-second budget (e.g., 50 ops/s). Apply the global 200 msg/s limit for all message types combined. Expose both limits and current rates in `/metrics`.

**Complexity:** M

**Dependencies:** 1.1, 1.2.

---

### 2.3 Deadlock Detection via Context Timeout

**Why it matters:** Operations that hold a lock for more than the configured `holdMs` but less than the 1-hour ceiling can still cause visible latency spikes. A global timeout (e.g., `context.WithTimeout` of 1 hour on every `primitiveOp` goroutine) ensures no goroutine is orphaned indefinitely even if the client disconnects mid-operation without cleanup.

**Current limitation:** Each `primitiveOp` goroutine is spawned with a context derived from the connection's context. If the connection is closed, the context is cancelled. However, blocking calls inside the primitive (e.g., `sem.Acquire()`) use the plain `Acquire()` method, not `AcquireContext()`. A disconnected client's goroutine continues blocking until it is eventually woken.

**Business/technical impact:** Prevents goroutine leaks on abnormal client disconnect. The goroutine accumulation metric becomes an early warning signal for stuck primitives.

**Implementation:** Replace all `Acquire()`, `Lock()`, `RLock()`, `Wait()` calls inside operation handlers with their `*Context` variants, using the connection context. Add a `context.WithTimeout(connCtx, time.Hour)` as an absolute safety ceiling.

**Complexity:** M

**Dependencies:** 2.2 (context propagation framework).

---

### 2.4 Metrics Histogram Configurable Buckets

**Why it matters:** The current histogram boundaries are hardcoded as `[100µs, 500µs, 1ms, 5ms, 10ms, 50ms, 100ms, 500ms, 1s]`. Applications where p99 lock wait is 10 µs need finer resolution at the low end. Applications where locks are held for seconds need coarser buckets at the high end.

**Current limitation:** `histBoundaries` slice in `internal/metrics/metrics.go` is a package-level constant. There is no way to configure it at server startup.

**Business/technical impact:** Misconfigured histogram buckets cause all observations to pile into a single bucket, losing latency distribution information.

**Implementation:** Add `HistogramBuckets []time.Duration` to `web.Config`. Default to the current boundaries if empty. Pass the bucket configuration to `MetricsCollector.RegisterPrimitive`.

**Complexity:** M

**Dependencies:** None.

---

### 2.5 Persistent Audit Log (Ring Buffer to Disk, JSON Lines Format)

**Why it matters:** The `Scheduler` maintains an in-memory ring buffer of the last 1000 events. These are lost on process restart. Operators need an audit trail for debugging: "Which connection created primitive X? When was it deleted? How many times was it acquired in the last hour?"

**Current limitation:** Events exist only in memory. There is no disk persistence or structured export.

**Business/technical impact:** Without persistent audit logs, postmortem analysis of production incidents is impossible. JSON Lines format allows direct ingestion by log aggregation tools without a parser.

**Implementation:** Add `AuditLogPath` to `web.Config`. Spawn an audit log writer goroutine that reads from a dedicated `auditCh` channel (separate from `eventCh`) and appends JSON Lines to the file. Include `timestamp`, `connID`, `type`, `primitiveID`, and `message` in each line.

**Complexity:** M

**Dependencies:** 2.1 (structured logging foundation).

---

### 2.6 Graceful WebSocket Connection Draining

**Why it matters:** The current shutdown sequence calls `server.Shutdown(ctx)` which stops accepting new HTTP connections. However, existing WebSocket connections are abruptly terminated when the context expires. Clients see a WebSocket error 1006 (abnormal closure) instead of 1001 (going away).

**Current limitation:** `web/server.go` `Shutdown` calls `httpServer.Shutdown(ctx)` without first sending `CloseMessage` to all active WebSocket connections.

**Business/technical impact:** Abrupt WebSocket closure triggers client-side reconnect logic immediately. A 1001 "going away" close frame allows clients to display a maintenance message and delay reconnection.

**Implementation:** Before calling `httpServer.Shutdown`, iterate over all active connections (tracked in a `sync.Map`), send `websocket.CloseMessage` with close code 1001, then wait up to 5 s for all connections to close cleanly before falling back to the hard shutdown.

**Complexity:** M

**Dependencies:** None.

---

### 2.7 Prometheus Scraping Integration Test

**Why it matters:** The `/metrics` endpoint emits Prometheus text format but is never tested against an actual Prometheus client library to verify correctness of the exposition format. Invalid format causes Prometheus to drop all metrics silently.

**Current limitation:** `web/server_test.go` tests HTTP status 200 for `/metrics` but does not parse the response with `expfmt.TextToMetricFamilies`.

**Business/technical impact:** A format regression would silently drop all metrics from Prometheus without alerting operators.

**Implementation:** Add an integration test that uses `github.com/prometheus/common/expfmt` to parse the `/metrics` response and assert that expected metric families exist with the correct metric types.

**Complexity:** S

**Dependencies:** None (test-only dependency on `github.com/prometheus/common`).

---

## Phase 3 — Advanced Primitive Features (Sprint 5–6, ~2 weeks)

**Goal:** Extend the primitive library with variants and improvements that address known algorithmic limitations.

---

### 3.1 Fair RWLock Variant (FIFO Ordering Across Readers and Writers)

**Why it matters:** The current `RWLock` is writer-preference: once a writer is queued, new readers must also queue, preventing writer starvation. However, this creates reader starvation under sustained write load. A FIFO-ordered variant grants access strictly in arrival order, ensuring neither readers nor writers starve.

**Current limitation:** The single-queue FIFO model is not yet implemented. The current dual-queue design cannot provide strict FIFO across reader/writer boundaries without significant restructuring.

**Business/technical impact:** Workloads with bursty writes (e.g., cache invalidation patterns) will see more predictable latency with a fair variant.

**Implementation:** Implement `FairRWLock` in a new file `internal/primitives/fair_rwlock.go`. Use a single `WaiterQueue` where each node carries a `kind` field (`reader` or `writer`). On release, dequeue nodes in order: if the head is a reader, dequeue all consecutive readers; if the head is a writer, dequeue exactly one writer.

**Complexity:** L

**Dependencies:** Core `WaiterQueue` (already implemented).

---

### 3.2 Reader-Preference RWLock Variant

**Why it matters:** Some workloads are read-dominated (e.g., configuration lookup, read-heavy caches). For these, the writer-preference policy adds unnecessary queuing overhead when writers arrive during a read burst. A reader-preference variant allows new readers to acquire even when writers are waiting, prioritizing throughput over write latency.

**Current limitation:** Only one RWLock variant exists. Users with read-heavy workloads have no alternative.

**Business/technical impact:** Read-heavy workloads can see 2–3× throughput improvement by eliminating unnecessary writer-wakeup overhead.

**Implementation:** `ReaderPrefRWLock` in `internal/primitives/reader_pref_rwlock.go`. Remove the writer-queue check from the read-acquisition fast path. Use a separate `writerHeld` flag to block readers only when a writer is actively holding the lock.

**Complexity:** M

**Dependencies:** 3.1 (the implementation patterns are related).

---

### 3.3 Priority Semaphore (Weighted Acquire)

**Why it matters:** Standard semaphores have no notion of priority. A high-priority request and a low-priority background job compete equally for slots. A weighted semaphore lets callers specify a priority level; higher-priority waiters are dequeued first.

**Current limitation:** `WaiterQueue` uses FIFO ordering. There is no priority-ordering mechanism.

**Business/technical impact:** Enables request prioritization in rate-limiting scenarios (e.g., premium vs. standard API tier).

**Implementation:** `PrioritySemaphore` backed by a min-heap (priority queue) of waiters. Requires a priority-aware replacement for `WaiterQueue`.

**Complexity:** L

**Dependencies:** None (new data structure required).

---

### 3.4 `Barrier.WaitN` for Variable Parties

**Why it matters:** The current `Barrier` requires all goroutines to call `Wait`. There is no way to say "wait for at least N out of M goroutines to arrive." This is useful for partial-barrier patterns (e.g., quorum-style checkpoints).

**Current limitation:** `parties` is fixed at construction time and cannot be changed without calling `Reset`.

**Business/technical impact:** Enables more flexible parallel algorithm implementations.

**Complexity:** M

**Dependencies:** None.

---

### 3.5 `Once.Reset` Safety Documentation and Tests

**Why it matters:** `Once.Reset` is safe to call only when no goroutines are blocked in `Do`. Calling `Reset` while another goroutine is executing `f()` inside `Do` leads to a subtle race: the next caller may call `f()` again before the first call completes, violating the "exactly once" contract.

**Current limitation:** `once.go` has a comment stating `Reset` exists. There is no documentation of the race condition and no test verifying the behavior.

**Business/technical impact:** Misuse of `Reset` in concurrent code can silently execute initialization logic twice, potentially causing database double-initialization or resource leaks.

**Implementation:** Add godoc warning. Add a test that calls `Reset` after `Do` completes (safe) and documents why calling `Reset` concurrently with `Do` is unsafe. Consider adding a `sync.WaitGroup`-based guard in `Reset` that waits for any in-progress `Do` to complete.

**Complexity:** S

**Dependencies:** None.

---

### 3.6 Singleflight Automatic Timeout and `Forget` After Configurable Duration

**Why it matters:** If `fn` passed to `Group.Do` hangs indefinitely, all callers with the same key block forever. There is currently no mechanism to automatically abandon an in-flight call and allow subsequent callers to retry.

**Current limitation:** `Forget(key)` must be called manually. There is no timeout on how long an in-flight call can be deduplicated.

**Business/technical impact:** A single slow upstream call can cause cascading timeouts in all callers deduplicating against it.

**Implementation:** Add `DoWithTimeout(key string, fn func() (interface{}, error), ttl time.Duration)` that automatically calls `Forget(key)` after `ttl` if `fn` has not completed.

**Complexity:** M

**Dependencies:** None.

---

## Phase 4 — Developer Experience (Sprint 7–8, ~2 weeks)

**Goal:** Make the library pleasant to use from Go programs and from the command line.

---

### 4.1 Go SDK Client Library (`pkg/client/`)

**Why it matters:** Currently the only way to interact with the server is through a raw WebSocket connection with hand-crafted JSON messages. There is no typed Go client. This creates high friction for users who want to programmatically manage primitives from a Go program.

**Current limitation:** No Go client package exists. Users must implement WebSocket dialing, message serialization, response correlation, and error handling themselves.

**Business/technical impact:** Reduces integration time from hours to minutes. Enables automated testing of server behavior from Go programs.

**Implementation:** Create `pkg/client/` with a `Client` struct that wraps a WebSocket connection and exposes typed methods: `CreateMutex(ctx, id, name)`, `Lock(ctx, id, holdMs)`, `Unlock(ctx, id)`, etc. Include automatic reconnection and response correlation by `id`.

**Complexity:** L

**Dependencies:** Phase 1 (stable API), Phase 2 (context propagation).

---

### 4.2 CLI Tool (`cmd/syncctl/`)

**Why it matters:** Operators and developers need a way to inspect and manipulate primitives from the command line without writing code or using a browser. A CLI enables scripting, automation, and debugging in headless environments.

**Current limitation:** No CLI exists. The only interfaces are the browser dashboard and raw WebSocket.

**Business/technical impact:** Enables operators to manually release a stuck lock, inspect primitive stats, or create primitives as part of a deployment script.

**Implementation:** Create `cmd/syncctl/` using `flag` or a minimal CLI framework. Commands: `list`, `create <type> <id>`, `op <id> <op>`, `delete <id>`, `stats <id>`. The CLI uses the `pkg/client/` SDK.

**Complexity:** M

**Dependencies:** 4.1.

---

### 4.3 Improved Example Programs

**Why it matters:** The current examples in `examples/` are minimal. They demonstrate the API but not realistic use cases. Developers learning the library need examples that show patterns like worker pools, producer-consumer pipelines, and initialization guards.

**Current limitation:** Examples show single-goroutine usage without demonstrating concurrency patterns.

**Business/technical impact:** Good examples reduce the time-to-first-value for new users.

**Complexity:** S per example

**Dependencies:** None.

---

### 4.4 Configuration via Environment Variables

**Why it matters:** Container-native deployments (Kubernetes, Docker Compose) configure applications via environment variables, not CLI flags. Supporting both allows operators to configure the server in Kubernetes without modifying the Deployment spec for every setting.

**Current limitation:** All configuration is via CLI flags only.

**Business/technical impact:** Eliminates the need for custom entrypoint scripts in container deployments.

**Implementation:** Add environment variable fallbacks for all CLI flags: `SYNCPRIM_ADDR`, `SYNCPRIM_ORIGINS`, `SYNCPRIM_API_KEY`, `SYNCPRIM_TLS_CERT`, `SYNCPRIM_TLS_KEY`, `SYNCPRIM_MAX_CONNS`, `SYNCPRIM_SNAPSHOT_PATH`. CLI flags take precedence over env vars.

**Complexity:** S

**Dependencies:** None.

---

## Phase 5 — Frontend UX Improvements (Sprint 9–10, ~2 weeks)

**Goal:** Make the dashboard accessible, mobile-friendly, and useful as a monitoring tool.

---

### 5.1 Dark Mode Toggle

**Why it matters:** Many developers work in dark mode. The current dashboard has a fixed light theme. Extended use of the dashboard in a bright white theme causes eye strain.

**Implementation:** Use CSS custom properties (`--bg`, `--fg`, `--accent`) for all colors. Toggle between light and dark themes by setting `data-theme` on `<html>`. Persist preference in `localStorage`. Respect `prefers-color-scheme` media query for the default.

**Complexity:** S

**Dependencies:** None.

---

### 5.2 Mobile Responsive Improvements

**Why it matters:** The canvas-based visualization uses fixed pixel dimensions that do not adapt to small screens. On mobile, the dashboard is unusable.

**Implementation:** Make the canvas responsive using `ResizeObserver`. Replace fixed-width layouts with CSS flexbox/grid. Add touch event handling on the canvas for pan/zoom.

**Complexity:** M

**Dependencies:** None.

---

### 5.3 Accessibility: ARIA Labels, Keyboard Navigation, Screen Reader Support

**Why it matters:** The current dashboard has no ARIA attributes. The canvas visualization is entirely inaccessible to screen readers. Users who rely on keyboard navigation cannot interact with the controls.

**Business/technical impact:** Required for compliance in organizations with accessibility policies (WCAG 2.1 AA). Also improves usability for all users via keyboard shortcuts.

**Implementation:** Add `role`, `aria-label`, and `aria-live` attributes to all interactive elements. Make the primitive table keyboard-navigable. Provide a text-based fallback view of the visualization for screen readers.

**Complexity:** M

**Dependencies:** None.

---

### 5.4 Primitive History Timeline Chart

**Why it matters:** The current canvas view shows only the current state. Operators want to see how a primitive's state changed over time — when were contention peaks? how often did waits time out in the last 5 minutes?

**Implementation:** Use `Chart.js` (or a canvas-based implementation) to render a rolling 5-minute sparkline of `acquires_total` and `waits_total` per primitive. Data sourced from the WebSocket `update` messages, maintained in a ring buffer in JavaScript.

**Complexity:** M

**Dependencies:** None.

---

### 5.5 WebSocket Connection Quality Indicator

**Why it matters:** When the WebSocket connection is degraded (high latency, message drops), the dashboard appears frozen. Users cannot distinguish between "nothing is happening" and "connection is broken."

**Implementation:** Display round-trip latency (ping/pong timing in JavaScript), message rate (messages/sec), and last-update timestamp. Color-code the indicator: green (<100 ms RTT), yellow (100–500 ms), red (>500 ms or stale).

**Complexity:** S

**Dependencies:** None.

---

### 5.6 Export Metrics as CSV/JSON from Browser

**Why it matters:** Operators want to export a snapshot of primitive statistics for offline analysis or sharing in a bug report.

**Implementation:** Add an "Export" button that serializes the current `state` snapshot to JSON and triggers a browser download. Add a "Export CSV" option that flattens the metrics table to CSV for spreadsheet analysis.

**Complexity:** S

**Dependencies:** None.

---

## Phase 6 — Enterprise Features (Sprint 11–12, ~2 weeks)

**Goal:** Add multi-tenancy, role-based access, and Kubernetes deployment support.

---

### 6.1 Multi-Tenant Namespacing

**Why it matters:** Currently, each WebSocket connection has its own isolated primitive namespace. This works for single-user scenarios but does not support team environments where multiple users need to share primitives (e.g., a distributed lock shared between CI jobs).

**Implementation:** Add a `namespace` field to `Config`. All primitive IDs are prefixed with `<namespace>/`. Different clients using the same namespace share the same primitive instances. Access to a namespace requires the API key configured for that namespace.

**Complexity:** L

**Dependencies:** 6.3 (JWT authentication provides per-namespace tokens).

---

### 6.2 Role-Based Access Control

**Why it matters:** Not all users of a shared namespace should have the same permissions. A viewer should be able to read stats but not acquire locks. An operator can perform operations. An admin can create and delete primitives.

**Implementation:** Define three roles: `admin` (create/delete), `operator` (create/delete + operate), `viewer` (read stats only). Encode the role in the JWT claims (Phase 6.3). Enforce role checks in the message dispatch switch in `web/server.go`.

**Complexity:** M

**Dependencies:** 6.3.

---

### 6.3 JWT Authentication (Replace Simple API Key)

**Why it matters:** A single static API key cannot encode user identity, roles, expiry, or namespace scope. JWTs provide all of these in a standard, verifiable format.

**Implementation:** Accept `Authorization: Bearer <jwt>` where the JWT is signed with a configurable HMAC-SHA256 secret. Claims: `sub` (user ID), `role` (`admin`/`operator`/`viewer`), `namespace`, `exp`. Reject tokens with invalid signatures or expired `exp`.

**Complexity:** M

**Dependencies:** None (implement with `golang.org/x/crypto` or a minimal JWT library).

---

### 6.4 Kubernetes Deployment Manifests

**Why it matters:** Organizations running Kubernetes need standard deployment artifacts: a `Deployment`, `Service`, `HorizontalPodAutoscaler`, `ConfigMap` for configuration, and a `ServiceMonitor` for Prometheus operator.

**Implementation:** Create `deploy/kubernetes/` containing:
- `deployment.yaml` — replicas=2, readinessProbe on `/readyz`, livenessProbe on `/healthz`, resource limits
- `service.yaml` — ClusterIP service on port 8085
- `hpa.yaml` — scale on CPU and memory
- `configmap.yaml` — server configuration
- `servicemonitor.yaml` — Prometheus operator scrape config

**Complexity:** M

**Dependencies:** 4.4 (environment variable configuration makes container config cleaner).

---

### 6.5 Helm Chart for Production Deployment

**Why it matters:** A Helm chart allows teams to deploy the server with a single `helm install` command, parameterized for their environment (TLS, API key, resource limits, replica count).

**Implementation:** Create `deploy/helm/syncprimitives/` with standard Helm chart structure. Parameterize: `replicaCount`, `image.tag`, `config.apiKey`, `config.allowedOrigins`, `tls.enabled`, `resources`, `hpa.enabled`.

**Complexity:** M

**Dependencies:** 6.4.

---

## Phase 7 — Scalability and Performance (Sprint 13–14, ~2 weeks)

**Goal:** Prepare the system for high-throughput production workloads and multi-instance deployments.

---

### 7.1 Horizontal Scaling via Shared State (Redis/etcd)

**Why it matters:** The current architecture is single-process. Primitive state lives in memory in one Go process. Running multiple replicas for availability would require shared state. Redis or etcd provide distributed coordination.

**Current limitation:** All primitives are in-memory maps. There is no mechanism to synchronize state across multiple server instances.

**Business/technical impact:** Single-instance deployment creates a single point of failure. Horizontal scaling enables high availability.

**Complexity:** L (research spike first recommended)

**Dependencies:** 6.1 (namespacing is required to partition state in Redis).

---

### 7.2 WebSocket Compression (permessage-deflate)

**Why it matters:** The WebSocket `update` messages contain the full state of all primitives and can be large (tens of kilobytes when many primitives are registered). Enabling per-message deflate compression can reduce bandwidth by 60–80% for typical payloads.

**Current limitation:** `gorilla/websocket` supports permessage-deflate but it is not enabled. The `Upgrader` uses default settings.

**Business/technical impact:** Reduces bandwidth costs and improves perceived latency on congested networks.

**Implementation:** Enable `EnableCompression: true` in the `websocket.Upgrader`. Verify correctness with browser WebSocket clients.

**Complexity:** S

**Dependencies:** None.

---

### 7.3 Delta Updates (Only Changed Primitives in Broadcasts)

**Why it matters:** Every 100 ms, the server broadcasts the full state of all primitives to all clients. With 100 primitives, each message is ~50 KB. With 1000 connections, this is 50 MB/s of serialization and network I/O for state that has not changed.

**Current limitation:** `broadcastUpdates` in `scheduler.go` always sends `GetPrimitives()` which returns all primitives.

**Business/technical impact:** 90%+ reduction in bandwidth and CPU for serialization. Enables scaling to more primitives and more connections.

**Implementation:** Add a version counter to each `PrimitiveInfo`. Track the last version sent per client. Only include primitives whose version is newer than the last sent. Include a `generation` field in the update message for clients to detect missed updates and request a full refresh.

**Complexity:** M

**Dependencies:** None.

---

### 7.4 Benchmark Regression Tracking in CI

**Why it matters:** Performance regressions are easy to introduce accidentally. Without automated comparison, they go undetected until they cause production incidents.

**Implementation:** Add a CI job that runs benchmarks on every PR, compares results against the main branch using `benchstat`, and fails if any benchmark regresses by more than 20% (`-delta-test none -confidence 0.95`).

**Complexity:** M

**Dependencies:** None.

---

### 7.5 Object Pool for Waiter Nodes (Reduce GC Pressure)

**Why it matters:** Every `Lock()`, `Acquire()`, and `Wait()` call allocates a `Waiter` struct on the heap. Under high contention, this creates significant GC pressure. A `sync.Pool` for `Waiter` nodes eliminates most of these allocations.

**Current limitation:** `NewWaiter()` always allocates. `BenchmarkMutexContended` shows 0 allocs/op currently because the test does not saturate the queue; actual production workloads under contention allocate heavily.

**Business/technical impact:** Reduces GC pause frequency under high-contention workloads. P99 latency improvement expected.

**Implementation:** Add a `var waiterPool = sync.Pool{New: func() interface{} { return &Waiter{ch: make(chan struct{}, 1)} }}`. Add a `releaseWaiter(w)` function that resets all fields and returns the waiter to the pool. Ensure all code paths that dequeue a waiter call `releaseWaiter`.

**Complexity:** M (requires careful pool hygiene to avoid returning an in-use waiter)

**Dependencies:** None.

---

### 7.6 Memory Profiling and Heap Size Optimization

**Why it matters:** The scheduler's `events` slice grows unboundedly until it is capped at 1000 entries, then shifts the slice (allocating a new backing array). The `goroutines` map holds `GoroutineInfo` structs that include a `WaitTime` field accumulating for the lifetime of a goroutine. Under heavy load, these maps can consume significant heap.

**Implementation:** Profile with `go tool pprof` under the `TestWebSocketLoad` scenario. Identify the top allocators. Optimize the events ring buffer to use a pre-allocated circular array rather than a slice with shifting.

**Complexity:** M

**Dependencies:** None.

---

## Summary Table

| Phase | Sprints | Items | Priority |
|-------|---------|-------|----------|
| 1 — Security & Stability | 1–2 | 7 items | P0 |
| 2 — Observability & Reliability | 3–4 | 7 items | P1 |
| 3 — Advanced Primitives | 5–6 | 6 items | P1–P2 |
| 4 — Developer Experience | 7–8 | 4 items | P2 |
| 5 — Frontend UX | 9–10 | 6 items | P2 |
| 6 — Enterprise Features | 11–12 | 5 items | P2–P3 |
| 7 — Scalability & Performance | 13–14 | 6 items | P2–P3 |

Items in Phase 1 must be completed before v0.2.0 release. Phases 2–3 target v0.3.0. Phases 4–7 target v1.0.0.
