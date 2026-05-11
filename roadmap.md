# Product Roadmap

**Project:** Advanced Synchronization Primitives
**Module:** `github.com/sanskar/syncprimitives`
**Current Version:** v0.2.0
**Last Updated:** 2026-05-11

This roadmap reflects the repository's shipped state on `origin/main` and the remaining open work. The older per-ticket design narratives are preserved in `tickets/` for historical reference.

---

## Current State

### Shipped since the original v0.1.0 planning pass

#### Security and reliability

- Input length validation, `holdMs` clamping, and Bearer-header-only API key auth are live.
- Snapshot persistence now uses a versioned envelope and still accepts the legacy unversioned format.
- CI runs `gosec`, blocking primitive operations use context-backed timeouts, and shutdown drains WebSocket clients cleanly.
- `CondVar.WaitTimeout` accounting and per-connection operation rate limiting are implemented and covered by tests.

#### Core product surface

- `FairRWLock` is implemented, registered in the server, and exposed in the dashboard.
- The Go SDK client ships in `pkg/client`.
- The `syncctl` CLI ships in `cmd/syncctl`, including JWT token generation for testing and automation.

#### Enterprise and observability

- HS256 JWT auth, namespace-scoped sharing, and RBAC for `admin` / `operator` / `viewer` are live.
- Persistent NDJSON audit logging with rotation controls is live.
- Histogram buckets are configurable through server config and surfaced in health output.

#### Performance work

- Primitive broadcasts use delta updates instead of always sending the full primitive set.
- WebSocket compression is enabled by default and can be disabled explicitly.
- CI compares benchmark output against a stored baseline.
- Contention-path waiter allocation now uses pooling.

---

## Remaining Open Work

### Near-term backlog

1. `TICKET-011` — dark mode and theme persistence for the dashboard.
2. `TICKET-015` — Kubernetes deployment manifests and related packaging.

### Still valuable, but not currently tracked as active tickets

1. Better example programs for realistic concurrency patterns.
2. Environment-variable configuration parity with CLI flags.
3. Frontend UX improvements beyond dark mode: responsiveness, accessibility, export flows, and connection-quality indicators.
4. Packaging follow-ups such as a Helm chart.
5. Longer-horizon scale-out work such as shared-state backends and deeper memory profiling.

---

## Tracking Guidance

- `todo.md` is the short list of work that is still open.
- Ticket files under `tickets/` are historical planning documents; shipped tickets are marked accordingly.
- `README.md` should describe the current public feature set, not speculative future work.
