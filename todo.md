# TODO

**Project:** Advanced Synchronization Primitives
**Updated:** 2026-05-11
**Current Sprint:** Post-v0.2.0 backlog refresh

This file tracks only work that is still open on `origin/main`. Historical ticket writeups remain under `tickets/`.

---

## Open Backlog

### P2 — Product and Packaging

- [ ] **Dark mode for dashboard** — add theme variables, persistence, and `prefers-color-scheme` defaults. Ticket: TICKET-011.
- [ ] **Kubernetes manifests** — create deployment manifests for the server, service, autoscaling, and metrics scraping. Ticket: TICKET-015.

---

## Shipped on `origin/main`

### Security and Stability

- [x] Input length validation and `holdMs` validation/clamping. Tickets: TICKET-001, TICKET-002.
- [x] Bearer-header-only API key authentication. Ticket: TICKET-003.
- [x] Improved `WaitGroup` negative-counter panic diagnostics. Ticket: TICKET-004.
- [x] Snapshot versioning with legacy-format compatibility. Ticket: TICKET-005.
- [x] `gosec` CI integration. Ticket: TICKET-006.
- [x] `CondVar.WaitTimeout` elapsed-time accounting fix. Ticket: TICKET-007.
- [x] Per-connection `primitiveOp` rate limiting. Ticket: TICKET-008.
- [x] Graceful WebSocket draining on shutdown. Ticket: TICKET-009.
- [x] Context-backed operation timeouts for blocking primitive operations. Ticket: TICKET-010.

### Primitives, SDK, and CLI

- [x] Fair FIFO `FairRWLock` primitive exposed through the server and UI. Ticket: TICKET-012.
- [x] Go SDK client in `pkg/client`. Ticket: TICKET-013.
- [x] `syncctl` CLI, including token generation support. Ticket: TICKET-014.

### Authentication, Multi-Tenancy, and Auditability

- [x] HS256 JWT authentication. Ticket: TICKET-016.
- [x] Persistent NDJSON audit logging with rotation controls. Tickets: TICKET-017, TICKET-021.
- [x] Multi-tenant namespace isolation with default namespace support. Ticket: TICKET-022.
- [x] Role-based access control for admin/operator/viewer roles. Ticket: TICKET-023.

### Performance and Observability

- [x] Delta WebSocket primitive updates. Ticket: TICKET-018.
- [x] WebSocket permessage-deflate compression toggle. Ticket: TICKET-019.
- [x] Configurable histogram buckets exposed through server config and health output. Ticket: TICKET-020.
- [x] Benchmark regression comparison in CI. Ticket: TICKET-024.
- [x] `sync.Pool` waiter reuse to reduce contention-path allocation pressure. Ticket: TICKET-025.

---

## Historical Notes

- The original v0.1.0 pre-release tracker became stale after the feature wave merged into `origin/main`.
- Use `roadmap.md` for current directional planning and the individual files in `tickets/` for historical design context.
