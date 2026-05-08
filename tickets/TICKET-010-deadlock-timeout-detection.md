# TICKET-010: Deadlock and Goroutine Leak Prevention via Context Timeouts

**Type:** improvement
**Priority:** P1
**Estimate:** M (4 days)
**Epic:** Observability and Reliability
**Labels:** p1, sprint-2, reliability, primitives, context, goroutine-leak
**Status:** TODO

## Problem Statement

When a WebSocket client disconnects (connection closed, network failure, browser tab closed), the server's per-connection goroutines should exit promptly. Currently, `handlePrimitiveOp` goroutines call the primitive's blocking methods (e.g., `sem.Acquire()`, `mu.Lock()`, `barrier.Wait()`) in their non-context variants. These methods block indefinitely regardless of whether the client is still connected.

Example scenario:
1. Client A creates a `Semaphore` with capacity 1 and acquires it with `holdMs: 3600000` (1 hour).
2. Client B creates the same-named semaphore (impossible — each connection has its own namespace — but within one connection), calls `acquire`, and the goroutine blocks on `sem.Acquire()`.
3. Client B's browser tab is closed. The WebSocket connection closes. The connection's context is cancelled.
4. The blocked `sem.Acquire()` goroutine continues blocking because it uses the non-context variant.

This creates a goroutine leak: goroutines that are blocked on primitive operations and will never be unblocked because the client is gone.

Additionally, even if the connection is still alive, a `primitiveOp` with no response (e.g., because the primitive is held by a deadlocked goroutine) should time out after a maximum duration. The absolute upper bound should be 1 hour (matching the maximum `holdMs`).

## Context

In `web/server.go`, each `primitiveOp` spawns a goroutine:
```go
case "primitiveOp":
    go func() {
        defer recover() // existing panic recovery
        // ...
        switch op {
        case "lock":
            mu.Lock()  // BUG: blocks indefinitely, ignores connection context
        case "acquire":
            sem.Acquire()  // BUG: same
        case "wait":
            barrier.Wait()  // BUG: same
        // ...
        }
        // auto-release after holdMs...
    }()
```

The connection context `connCtx` is created at the start of `HandleWebSocket` and cancelled when the connection closes. It is NOT propagated into the goroutines spawned by `handlePrimitiveOp`.

All 8 primitives have context-cancellable variants: `LockContext`, `AcquireContext`, `RLockContext`, `WaitContext` — but they are not used in the operation handler.

## Goals

1. Propagate the connection context into all `primitiveOp` goroutines.
2. Wrap the connection context with a 1-hour absolute timeout (`context.WithTimeout(connCtx, time.Hour)`) as a safety ceiling.
3. Replace all non-context blocking calls in `handlePrimitiveOp` with their `*Context` variants.
4. When a context is cancelled mid-operation, send an error response if the connection is still alive.
5. Add test `TestConnectionContextCancelledUnblocksGoroutine`.

## Non-Goals

- Implementing deadlock detection between primitives (cycles in the wait-for graph). This is a much harder problem, tracked in the roadmap Phase 2.
- Changing the behavior of `Hold` goroutines (the goroutine that auto-releases after `holdMs`). The hold goroutine is separate from the acquisition goroutine and should time out based on the connection context.
- Context propagation into the primitives themselves (they receive contexts from callers; the primitives are context-unaware by design).

## Technical Design

Pass the connection context to `handlePrimitiveOp`:

```go
// At the top of HandleWebSocket, create a per-connection context:
connCtx, connCancel := context.WithCancel(r.Context())
defer connCancel()

// When dispatching primitiveOp:
case "primitiveOp":
    go s.handlePrimitiveOp(connCtx, conn, payload, primitives)
```

In `handlePrimitiveOp`:
```go
func (s *Server) handlePrimitiveOp(
    connCtx context.Context,
    conn *websocket.Conn,
    payload primitiveOpPayload,
    primitives map[string]primEntry,
) {
    // Wrap with 1-hour absolute timeout
    ctx, cancel := context.WithTimeout(connCtx, time.Hour)
    defer cancel()

    defer func() {
        if r := recover(); r != nil {
            slog.Error("panic in primitiveOp", "recover", r)
            // send error if conn still alive
        }
    }()

    // All blocking calls use ctx:
    switch payload.Op {
    case "lock":
        if err := mu.LockContext(ctx); err != nil {
            sendError(conn, "lock cancelled: "+err.Error())
            return
        }
    case "acquire":
        if err := sem.AcquireContext(ctx); err != nil {
            sendError(conn, "acquire cancelled: "+err.Error())
            return
        }
    case "rlock":
        if err := rw.RLockContext(ctx); err != nil {
            sendError(conn, "rlock cancelled: "+err.Error())
            return
        }
    // ... etc.
    }

    // Auto-release after holdMs (also uses context)
    select {
    case <-ctx.Done():
        // Context cancelled before holdMs elapsed; release immediately
    case <-time.After(time.Duration(holdMs) * time.Millisecond):
        // holdMs elapsed; auto-release
    }
    // release primitive
}
```

## Backend Implementation

1. Add `connCtx context.Context` parameter to `handlePrimitiveOp` (or pass via a struct).
2. In `HandleWebSocket`, create `connCtx` from `r.Context()`.
3. Pass `connCtx` to all `go s.handlePrimitiveOp(...)` invocations.
4. Replace `mu.Lock()` with `mu.LockContext(ctx)` and handle the error.
5. Replace `mu.Unlock()` (which is non-blocking) with plain `mu.Unlock()` — context is not needed for release.
6. Replace `sem.Acquire()` with `sem.AcquireContext(ctx)`.
7. Replace `rw.RLock()` with `rw.RLockContext(ctx)`.
8. Replace `rw.Lock()` with `rw.LockContext(ctx)`.
9. Replace `barrier.Wait()` with `barrier.WaitContext(ctx)`.
10. Replace `wg.Wait()` with `wg.WaitContext(ctx)`.
11. For `CondVar.Wait(mu)`: CondVar does not have a `WaitContext` variant yet (see architecture-improvements.md W1.6). Use `cv.WaitTimeout(mu, time.Until(deadline))` as a temporary workaround, where `deadline = time.Now().Add(time.Hour)` derived from the context.
12. Update the `time.Sleep(holdMs)` in the auto-release goroutine to use `select { case <-ctx.Done(): ... case <-time.After(holdMs): ... }`.

## Frontend Implementation

No frontend changes. Context cancellation is transparent to the client.

## Database / State Changes

None.

## API Changes

When a context is cancelled mid-operation (e.g., connection dropped), the client no longer receives a success response. This was already the case (the connection is gone). The goroutine now exits promptly rather than lingering.

## Infrastructure Requirements

None.

## Edge Cases

- Connection closes while goroutine is holding a lock (during the `holdMs` sleep): the `select` on `ctx.Done()` fires, the goroutine releases the primitive and exits. Correct behavior.
- Context cancelled before the blocking call starts: `*Context` methods check for cancellation before blocking (via `select { case <-ctx.Done(): ... default: }` fast path). Return immediately.
- Multiple goroutines blocked on the same primitive, connection closes: all goroutines are woken (their contexts are cancelled), each attempts to send an error response. The connection is closed, so `conn.WriteMessage` returns an error. Log but do not panic.
- `Once.Do` — non-blocking, no context needed.
- `Singleflight.Do` — wraps `fn()` which is user-provided. The goroutine running `fn` cannot be context-cancelled unless `fn` itself accepts a context. This is a known limitation; document it.

## Failure Handling

- Context cancelled error from `*Context` method: send error response if connection is still alive, then return.
- `conn.WriteMessage` fails on closed connection: log at DEBUG level, return.
- `context.DeadlineExceeded` (1-hour absolute timeout): send error response with message "operation timed out after 1 hour", release primitive, return.

## Security Considerations

- Ensures that a client that connects, creates a large number of blocked goroutines, then disconnects, does not leave those goroutines running forever.
- The 1-hour absolute timeout ensures that even if `connCtx` is never cancelled (impossible in practice, but defensively), goroutines still exit eventually.

## Testing Plan

### Unit Tests

```go
func TestConnectionContextCancelledUnblocksGoroutine(t *testing.T) {
    ts, cleanup := newTestServer(t)
    defer cleanup()

    conn := dialWS(t, ts.URL+"/ws")
    drainOne(conn, time.Second)

    // Create semaphore with capacity 1
    sendJSON(conn, "createSemaphore", map[string]interface{}{
        "id": "sem1", "name": "sem1", "capacity": 1,
    })
    drainOne(conn, time.Second)

    // Acquire the semaphore (blocks for 5s)
    sendJSON(conn, "primitiveOp", map[string]interface{}{
        "id": "sem1", "op": "acquire", "holdMs": 5000,
    })
    drainOne(conn, time.Second) // success

    // Try to acquire again — this blocks because capacity is exhausted
    sendJSON(conn, "primitiveOp", map[string]interface{}{
        "id": "sem1", "op": "acquire", "holdMs": 5000,
    })

    // Close the connection before the blocked acquire completes
    conn.Close()

    // The blocked goroutine should exit promptly (within 500ms)
    // We verify by checking goroutine count doesn't increase permanently
    time.Sleep(500 * time.Millisecond)
    // Assert: server has no leaked goroutines for this connection
    // (difficult to test directly; use pprof or goroutine count comparison)
}
```

```go
func TestAbsoluteTimeoutUnblocksGoroutine(t *testing.T) {
    // This test would need to set a very short absolute timeout for testing.
    // Consider adding a Config.OperationTimeout field (default 1h, test uses 100ms).
}
```

### Integration Tests

Run `TestWebSocketLoad` and measure goroutine count before and after. Verify no goroutine leak after all connections close.

### E2E Tests

Manual: Create a barrier with parties=5. Open 4 WebSocket connections each calling `barrier.wait`. Close 2 connections. Verify the remaining 2 connections eventually receive an error (context cancelled) rather than hanging forever.

## Monitoring Requirements

- Log when a goroutine exits due to context cancellation (at DEBUG level to avoid noise):
  ```
  level=DEBUG msg="primitiveOp context cancelled" id="sem1" op="acquire" reason="connection closed"
  ```

## Logging Requirements

```
level=DEBUG msg="primitiveOp goroutine cancelled" id="sem1" op="acquire" err="context canceled"
level=WARN  msg="primitiveOp timed out (1-hour absolute limit)" id="lock1" op="lock"
```

## Metrics to Track

- `syncprim_op_context_cancellations_total` — new counter, incremented when a goroutine exits due to context cancellation.

## Rollback Plan

Remove the context parameter from `handlePrimitiveOp`, revert all `*Context` calls to their non-context variants, and remove the `select` on `ctx.Done()` in the auto-release code. Goroutine leak behavior returns. No data corruption.

## Acceptance Criteria

- [ ] All blocking primitive calls in `handlePrimitiveOp` use `*Context` variants
- [ ] Connection context is propagated to all `primitiveOp` goroutines
- [ ] 1-hour absolute timeout applied via `context.WithTimeout(connCtx, time.Hour)`
- [ ] Auto-release goroutine respects context cancellation via `select { ctx.Done(), time.After }`
- [ ] `TestConnectionContextCancelledUnblocksGoroutine` passes
- [ ] No goroutine leaks in `TestWebSocketLoad` after connections close

## Definition of Done

- [ ] Code reviewed and merged
- [ ] Tests passing with race detector
- [ ] Coverage maintained ≥70%
- [ ] Goroutine leak test passing
- [ ] CHANGELOG entry written
