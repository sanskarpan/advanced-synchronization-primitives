# TODO

**Project:** Advanced Synchronization Primitives
**Updated:** 2026-05-09
**Current Sprint:** Sprint 1

Items are organized by priority tier. P0 items must ship before any public release. P1 items target the next sprint. P2 items are backlog.

---

## P0 — This Sprint (Security, must ship before v0.2.0)

- [ ] **Add input length validation** — reject `id` and `name` fields exceeding 256 characters in all `create*` and `primitiveOp` handlers in `web/server.go`. Return `{"type":"error","payload":{"message":"id exceeds maximum length of 256 characters"}}`. Also validate `holdMs` is an integer type (reject float). Ticket: TICKET-001, TICKET-002.

- [ ] **Remove API key from URL query parameter** — delete the `r.URL.Query().Get("key")` fallback in `HandleWebSocket`. Only `Authorization: Bearer <key>` is accepted. Update README. Add a deprecation notice in CHANGELOG. Ticket: TICKET-003.

- [ ] **Add `WaitGroup.Add()` panic improvement** — current panic message is `"waitgroup: negative counter"`. Improve to include the current counter value and the delta: `"waitgroup: negative counter (current=%d, delta=%d)"`. Add test `TestWaitGroupNegativeAddPanics` that verifies the panic message. Ticket: TICKET-004.

- [ ] **Add snapshot version field** — wrap snapshot JSON in `{"version": 1, "primitives": {...}}`. On load, check version and skip loading with a `slog.Warn` if version is not 1. Prevents silent corruption when schema changes. Ticket: TICKET-005.

---

## P1 — Next Sprint (Observability and Reliability)

- [ ] **Add `gosec` to CI pipeline** — add `gosec` step to `.github/workflows/ci.yml`. Use `-severity medium -confidence medium` flags. Fix any issues it reports. Ticket: TICKET-006.

- [ ] **Fix `CondVar.WaitTimeout` elapsed time** — in `condvar.go`, `WaitTimeout` does not record `totalWaitTime` on the timeout path. Move `cv.totalWaitTime.Add(waitTime)` outside the `if signaled {}` block so it is always recorded. Add `TestCondVarWaitTimeoutElapsedTracking` that verifies `stats.TotalWaitTimeNs > 0` after a timeout. Ticket: TICKET-007.

- [ ] **Add operation-level rate limiting** — implement a separate per-connection `primitiveOp` rate limit (50 ops/s) in addition to the existing 200 msg/s global rate limit. A `primitiveOp` message counts toward both buckets. Ticket: TICKET-008.

- [ ] **Implement graceful WebSocket connection draining on shutdown** — before calling `httpServer.Shutdown`, send `websocket.CloseMessage` with close code 1001 to all active connections, then wait up to 5 s for them to close cleanly. Ticket: TICKET-009.

- [ ] **Add deadlock timeout detection** — replace all blocking `Acquire()`, `Lock()`, `RLock()`, `Wait()` calls inside `handlePrimitiveOp` goroutines with their `*Context` variants. Pass a context with `time.Hour` absolute timeout, derived from the connection context. Ticket: TICKET-010.

---

## P2 — Backlog

- [ ] **Dark mode for dashboard** — add CSS custom properties for colors, localStorage persistence, and `prefers-color-scheme` default. Ticket: TICKET-011.

- [ ] **Fair RWLock variant** — implement `FairRWLock` with FIFO ordering across readers and writers using a single waiter queue with `kind` field. Ticket: TICKET-012.

- [ ] **Go SDK client library** — create `pkg/client/` with typed methods for all WebSocket operations. Include automatic reconnection and request/response correlation. Ticket: TICKET-013.

- [ ] **CLI tool** — create `cmd/syncctl/` using the Go SDK client. Commands: `list`, `create`, `op`, `delete`, `stats`. Ticket: TICKET-014.

- [ ] **Kubernetes manifests** — create `deploy/kubernetes/` with Deployment, Service, HPA, ConfigMap, and ServiceMonitor. Ticket: TICKET-015.

- [ ] **JWT authentication** — replace static API key with HMAC-SHA256 signed JWT. Claims: `sub`, `role`, `namespace`, `exp`. Ticket: TICKET-016.

- [ ] **Persistent audit log** — write scheduler events to a JSON Lines file on disk. Add `AuditLogPath` to `web.Config`. Ticket: TICKET-017, TICKET-021.

- [ ] **Delta WebSocket updates** — only broadcast changed primitives (version-based diffing). Reduces bandwidth by 90%+ in steady state. Ticket: TICKET-018.

- [ ] **WebSocket permessage-deflate compression** — enable `EnableCompression: true` in the Gorilla WebSocket upgrader. Ticket: TICKET-019.

- [ ] **Configurable histogram buckets** — add `HistogramBuckets []time.Duration` to `web.Config`. Ticket: TICKET-020.

- [ ] **Multi-tenant namespacing** — add `namespace` field to Config; prefix all primitive IDs with `<namespace>/`. Ticket: TICKET-022.

- [ ] **Role-based access control** — define admin/operator/viewer roles enforced at message dispatch. Ticket: TICKET-023.

- [ ] **Benchmark regression tracking in CI** — run benchmarks on PR, compare with `benchstat`, fail on >20% regression. Ticket: TICKET-024.

- [ ] **Object pool for Waiter nodes** — use `sync.Pool` for `Waiter` allocation to reduce GC pressure under high contention. Ticket: TICKET-025.

---

## Completed (v0.1.0)

- [x] HTTP server timeouts (ReadHeader, Read, Write, Idle)
- [x] CORS origin allowlist
- [x] Per-connection sliding-window rate limit (200 msg/s)
- [x] TLS support
- [x] WebSocket ping/pong keepalive
- [x] 64 KiB read limit per message
- [x] Dropped broadcast counter
- [x] Configurable `holdMs` field (clamped [1, 5000])
- [x] Connection cap (MaxConns)
- [x] Structured logging with `log/slog`
- [x] `/metrics` Prometheus text endpoint
- [x] `/healthz` and `/readyz` endpoints
- [x] Context-cancellable variants for all blocking primitives
- [x] `CondVar.WaitFor` spurious-wakeup-safe helper
- [x] `Barrier` over-subscription guard
- [x] `sync.Locker` interface compliance checks
- [x] Exponential-backoff WebSocket reconnection in frontend
- [x] Toast notifications for server errors
- [x] CI pipeline with race detector and coverage gate
- [x] Multi-stage Dockerfile with non-root Alpine user
- [x] Fuzz tests for Semaphore and Barrier
- [x] 11 integration tests in `web/server_test.go`
- [x] `recover()` defers in all `handlePrimitiveOp` goroutines
- [x] `eventCh` decoupling to prevent nested-lock deadlock in scheduler
