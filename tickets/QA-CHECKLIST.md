# QA Testing Checklist

**Project:** Advanced Synchronization Primitives
**Version:** v0.2.0
**Updated:** 2026-05-09

This checklist is used before every release to verify all features work correctly. Testers should go through each section systematically and mark items as tested.

---

## 1. Server Startup and Configuration

- [ ] Server starts successfully with default settings (`go run ./cmd/server/...`)
- [ ] Server binds to the configured address (default `:8085`)
- [ ] `LOG_FORMAT=json` produces valid JSON log lines on stderr
- [ ] `LOG_FORMAT=text` (default) produces human-readable log lines
- [ ] `-addr :9090` changes the listen port
- [ ] `--tls-cert` and `--tls-key` enable TLS
- [ ] Server listens on HTTPS when TLS is configured
- [ ] `--api-key secret` enables authentication
- [ ] `--allowed-origins https://myapp.com` restricts WebSocket origins
- [ ] Server exits cleanly on SIGTERM (graceful shutdown)
- [ ] Server exits cleanly on SIGINT (Ctrl+C)
- [ ] Graceful shutdown sends close code 1001 to all active WebSocket connections (after TICKET-009)
- [ ] Server exits within 10 seconds of receiving shutdown signal

---

## 2. HTTP Endpoints

### GET /healthz
- [ ] Returns HTTP 200
- [ ] Response is valid JSON with `"status": "ok"`
- [ ] `"uptime"` field is a non-zero duration string
- [ ] `"dropped_broadcasts"` field is present and a non-negative integer
- [ ] Returns HTTP 200 even under load

### GET /readyz
- [ ] Returns HTTP 200 when server is ready
- [ ] Returns HTTP 503 when `draining` is set (after TICKET-009)

### GET /metrics
- [ ] Returns HTTP 200
- [ ] Response body contains `syncprim_` prefixed metrics
- [ ] Response is valid Prometheus text format (parseable by `expfmt`)
- [ ] `syncprim_acquires_total` counter present
- [ ] `syncprim_wait_duration_seconds` histogram present with correct bucket boundaries
- [ ] `syncprim_uptime_seconds` gauge present
- [ ] `syncprim_dropped_broadcasts_total` counter present

### GET /
- [ ] Returns the dashboard HTML
- [ ] Dashboard loads without JavaScript errors in browser console
- [ ] Dashboard connects to WebSocket automatically on load
- [ ] Static assets (CSS, JS) load with correct Content-Type headers

---

## 3. WebSocket Connection

### Authentication
- [ ] No API key configured: connection accepted without Authorization header
- [ ] API key configured, no Authorization header: HTTP 401 response
- [ ] API key configured, wrong key in `Authorization: Bearer <wrong>`: HTTP 401
- [ ] API key configured, correct key in `Authorization: Bearer <key>`: connection accepted
- [ ] `?key=<apikey>` query parameter rejected with HTTP 401 (after TICKET-003)

### Rate Limiting
- [ ] 200 messages/second per connection: 201st message gets error response
- [ ] 50 primitiveOp/second per connection: 51st gets "operation rate limit exceeded" error (after TICKET-008)
- [ ] Rate limit window resets after 1 second

### Connection Cap
- [ ] `MaxConns: 5` and 6th connection attempt gets HTTP 503 (connection refused)

### Keepalive
- [ ] Connection survives idle for 60+ seconds (ping/pong maintains it)
- [ ] Connection drops if pong not received within 60 seconds of ping

### Message Size
- [ ] Message > 64 KiB causes connection to be closed
- [ ] Message exactly 64 KiB is processed (if otherwise valid)

### Initial State
- [ ] On connect, client receives `{"type":"state","payload":{...}}` message immediately

---

## 4. Primitive Operations — Mutex

- [ ] `createMutex` with valid `id` and `name` returns success
- [ ] `createMutex` with duplicate `id` returns error
- [ ] `primitiveOp {"op":"lock"}` locks the mutex and returns success
- [ ] `primitiveOp {"op":"lock"}` while already locked blocks and auto-releases after holdMs
- [ ] `primitiveOp {"op":"unlock"}` unlocks the mutex
- [ ] `primitiveOp {"op":"tryLock"}` on unlocked mutex returns success
- [ ] `primitiveOp {"op":"tryLock"}` on locked mutex returns success immediately with `acquired: false`
- [ ] `deletePrimitive` on mutex returns success
- [ ] Operation on deleted primitive returns error

---

## 5. Primitive Operations — RWLock

- [ ] `createRWLock` creates the primitive
- [ ] `primitiveOp {"op":"rlock"}` acquires read lock
- [ ] Multiple `rlock` operations can be active simultaneously
- [ ] `primitiveOp {"op":"lock"}` (write lock) blocks while any read lock is held
- [ ] After all read locks released, write lock acquires
- [ ] `primitiveOp {"op":"runlock"}` releases read lock
- [ ] `primitiveOp {"op":"unlock"}` releases write lock
- [ ] `tryRLock` on read-available lock returns success
- [ ] `tryRLock` while write lock held returns failure (not acquired)
- [ ] `tryLock` on unlocked RWLock succeeds
- [ ] `tryLock` while any lock held fails

---

## 6. Primitive Operations — Semaphore

- [ ] `createSemaphore` with `capacity: 5` creates semaphore
- [ ] `createSemaphore` with `capacity: 0` returns error
- [ ] `createSemaphore` with `capacity: -1` returns error
- [ ] `acquire` when slots available succeeds immediately
- [ ] `acquire` when all slots held blocks until a slot is released
- [ ] `release` increments available count and unblocks a waiter
- [ ] Over-release (more releases than acquires) returns error

---

## 7. Primitive Operations — CondVar

- [ ] `createCondVar` creates the primitive
- [ ] `wait` blocks and auto-returns after holdMs
- [ ] `signal` wakes one waiting goroutine
- [ ] `broadcast` wakes all waiting goroutines

---

## 8. Primitive Operations — Barrier

- [ ] `createBarrier` with `parties: 3` creates a 3-party barrier
- [ ] `createBarrier` with `parties: 0` returns error
- [ ] `wait` on barrier with parties=1 completes immediately
- [ ] Three concurrent `wait` calls on a parties=3 barrier all complete
- [ ] `break` causes all waiting goroutines to receive an error
- [ ] `reset` clears broken state and allows re-use

---

## 9. Primitive Operations — WaitGroup

- [ ] `createWaitGroup` creates the primitive
- [ ] `add` increments the counter
- [ ] `done` decrements the counter
- [ ] `wait` blocks until counter reaches 0
- [ ] Excess `done` calls (counter goes negative) return error

---

## 10. Primitive Operations — Once

- [ ] `createOnce` creates the primitive
- [ ] `do` executes (simulates) the action once
- [ ] Second `do` does not execute again (no response for action)
- [ ] `reset` allows next `do` to execute again

---

## 11. Primitive Operations — Singleflight

- [ ] `createSingleflight` creates the primitive
- [ ] `do` deduplicates concurrent calls with the same key
- [ ] `forget` removes the in-flight key

---

## 12. Input Validation (TICKET-001, TICKET-002)

- [ ] `id` of 256 characters is accepted
- [ ] `id` of 257 characters is rejected with error message containing "exceeds maximum length"
- [ ] `name` of 256 characters is accepted
- [ ] `name` of 257 characters is rejected
- [ ] `holdMs: 3600000` is accepted
- [ ] `holdMs: 3600001` is clamped to 3600000 with warning in response
- [ ] `holdMs: 0` defaults to 100 ms (no error)

---

## 13. Snapshot Persistence

- [ ] Server starts with empty state when no snapshot file exists
- [ ] After creating primitives, snapshot is written on graceful shutdown
- [ ] Server restores primitives from snapshot on restart
- [ ] Snapshot file format contains `"version": 1` envelope
- [ ] Legacy (unversioned) snapshot files are loaded with a warning log
- [ ] Snapshot with `"version": 99` is rejected, server starts with empty state

---

## 14. Dashboard UI

- [ ] Dashboard connects to WebSocket automatically
- [ ] "Connected" status indicator is shown
- [ ] Primitive list updates in real time as operations are performed
- [ ] Create panel allows creating all 8 primitive types
- [ ] Operation panel shows appropriate operations for each primitive type
- [ ] Toast notification appears for server errors
- [ ] "Reconnecting..." indicator appears on WebSocket disconnect
- [ ] Reconnection succeeds after server restart
- [ ] Frontend validates `capacity` as positive integer before sending
- [ ] Frontend validates `parties` as positive integer before sending
- [ ] Frontend validates `id` length ≤ 256 before sending (after TICKET-001)

### Dark Mode (after TICKET-011)
- [ ] Toggle button in header changes theme
- [ ] Dark mode preference persists on page reload
- [ ] System `prefers-color-scheme: dark` respected as default

---

## 15. Metrics Correctness

- [ ] After locking a mutex, `syncprim_acquires_total{type="Mutex"}` increments
- [ ] After a contended lock, `syncprim_waits_total{type="Mutex"}` increments
- [ ] Wait duration appears in the histogram bucket (e.g., `_bucket{le="0.001"}`)
- [ ] CondVar wait timeout is tracked in `TotalWaitTimeNs` (after TICKET-007)

---

## 16. Race Detector

- [ ] `go test -race -count=3 ./...` passes with zero race conditions
- [ ] `go test -race -count=3 ./internal/primitives/` with 10+ goroutines passes

---

## 17. Load Test

- [ ] `TestWebSocketLoad` passes with 50 connections × 20 cycles
- [ ] Error rate in load test < 5%
- [ ] No goroutine leaks after load test completes (goroutine count returns to baseline)
- [ ] Server CPU and memory are stable during load test

---

## 18. Security Tests

- [ ] See SECURITY-CHECKLIST.md for complete security validation

---

## 19. Graceful Shutdown

- [ ] All WebSocket connections receive close code 1001 on SIGTERM
- [ ] Server exits within 10 seconds of SIGTERM
- [ ] In-progress operations are cancelled (context cancellation) on shutdown (after TICKET-010)
- [ ] Snapshot is written on clean shutdown

---

## 20. Docker

- [ ] `docker build -t syncprimitives .` succeeds
- [ ] `docker run -p 8085:8085 syncprimitives` starts the server
- [ ] Dashboard accessible at `http://localhost:8085`
- [ ] Server runs as non-root user inside container
- [ ] Container memory usage ≤ 100 MB in idle state

---

## Sign-off

| Area | Tester | Date | Status |
|------|--------|------|--------|
| Server startup | | | |
| HTTP endpoints | | | |
| WebSocket connection | | | |
| All 8 primitives | | | |
| Input validation | | | |
| Snapshot persistence | | | |
| Dashboard UI | | | |
| Metrics | | | |
| Race detector | | | |
| Load test | | | |
| Security | | | |
| Docker | | | |
