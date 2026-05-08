# TICKET-008: Operation-Level Rate Limiting Per Connection

**Type:** feature
**Priority:** P1
**Estimate:** M (3 days)
**Epic:** Observability and Reliability
**Labels:** p1, sprint-2, security, web-server, rate-limiting
**Status:** TODO

## Problem Statement

The current rate limiting applies a sliding-window limit of 200 messages/second to all inbound WebSocket messages equally. This means a client can submit 200 `primitiveOp` messages per second, each spawning a goroutine that can sleep for up to 5,000 ms (or 3,600,000 ms after TICKET-002). With 200 ops/s and a 5 s hold time, a single connection can accumulate 200 × 5 = 1,000 blocked goroutines in the first 5 seconds alone.

More critically, the global 200 msg/s limit does not distinguish between:
- Cheap read operations (stats queries, once.Do, etc.)
- Expensive lock operations (mutex.lock, semaphore.acquire with long holdMs)

Each expensive operation spawns a goroutine, registers it with the scheduler, and holds a primitive for the duration of holdMs. The goroutine count accumulation is the primary resource concern, not message parsing throughput.

## Context

Current rate limiting in `web/server.go`:

```go
type connState struct {
    msgTimes []time.Time
    mu       sync.Mutex
}

func (s *Server) checkRateLimit(state *connState) bool {
    state.mu.Lock()
    defer state.mu.Unlock()
    now := time.Now()
    window := now.Add(-time.Second)
    // Remove old timestamps
    for len(state.msgTimes) > 0 && state.msgTimes[0].Before(window) {
        state.msgTimes = state.msgTimes[1:]
    }
    if len(state.msgTimes) >= 200 {
        return false // rate limit exceeded
    }
    state.msgTimes = append(state.msgTimes, now)
    return true
}
```

This runs on every message. When exceeded, the server sends an error response but does not close the connection.

## Goals

1. Add a separate, tighter per-connection rate limit for `primitiveOp` messages: 50 ops/second.
2. Keep the existing 200 msg/s limit for all messages combined.
3. `primitiveOp` messages count toward both the 50 ops/s limit and the 200 msg/s limit.
4. Expose both limits and current rates in `/metrics`.
5. When the ops-specific limit is exceeded, return an informative error response (different message from the generic rate limit error).

## Non-Goals

- Per-primitive-type rate limits (e.g., different limits for lock vs. acquire).
- Adaptive rate limits that adjust based on server load.
- Disconnecting clients that repeatedly exceed the rate limit (the current behavior of sending errors and continuing is correct for this ticket).
- Operation rate limits for read-only operations (stats queries are not `primitiveOp` messages).

## Technical Design

Extend `connState` with a second sliding-window counter:

```go
type connState struct {
    // Global message rate limit (200/s)
    msgTimes []time.Time
    // primitiveOp-specific rate limit (50/s)
    opTimes  []time.Time
    mu       sync.Mutex
}
```

Add a `checkOpRateLimit` function:
```go
const (
    maxMsgPerSecond = 200
    maxOpsPerSecond = 50
)

func (s *Server) checkOpRateLimit(state *connState) bool {
    state.mu.Lock()
    defer state.mu.Unlock()
    now := time.Now()
    window := now.Add(-time.Second)
    for len(state.opTimes) > 0 && state.opTimes[0].Before(window) {
        state.opTimes = state.opTimes[1:]
    }
    if len(state.opTimes) >= maxOpsPerSecond {
        return false
    }
    state.opTimes = append(state.opTimes, now)
    return true
}
```

In the message dispatch, for `primitiveOp` messages, call `checkOpRateLimit` before `checkRateLimit`:
```go
case "primitiveOp":
    if !s.checkOpRateLimit(connStateRef) {
        sendError(conn, "operation rate limit exceeded (max 50 ops/second per connection)")
        continue
    }
    // existing handler...
```

Add two server-level counters:
- `opRateLimitHits`: incremented when `checkOpRateLimit` returns false
- `msgRateLimitHits`: incremented when `checkRateLimit` returns false (rename existing counter)

## Backend Implementation

1. Add `opTimes []time.Time` to `connState`.
2. Implement `checkOpRateLimit(state *connState) bool`.
3. Define constants `maxMsgPerSecond = 200` and `maxOpsPerSecond = 50`.
4. Call `checkOpRateLimit` for `primitiveOp` messages before the operation handler.
5. Add `opRateLimitHits atomic.Int64` to `*Server`.
6. Expose `syncprim_op_rate_limit_hits_total` in `/metrics`.
7. Add `syncprim_op_rate_limit_hits_total` to `/healthz`.

## Frontend Implementation

Update the dashboard's client-side send logic to display a specific warning when it receives `"operation rate limit exceeded"`:
```javascript
if (payload.message.includes('operation rate limit exceeded')) {
    showWarning('Slow down: max 50 operations/second per connection');
    // Optionally: implement exponential backoff for op sends
}
```

## Database / State Changes

None.

## API Changes

- New error message format when ops rate limit exceeded: `{"type":"error","payload":{"message":"operation rate limit exceeded (max 50 ops/second per connection)"}}`.
- Existing clients that handle generic error responses are unaffected.

## Infrastructure Requirements

None.

## Edge Cases

- Connection sends exactly 50 `primitiveOp` messages in 1 second and 1 additional one: the 51st is rejected with the ops-specific error. The first 50 succeed.
- Connection sends 200 non-`primitiveOp` messages then 1 `primitiveOp`: the `primitiveOp` hits the global 200 msg/s limit before the ops-specific limit. Error message says "rate limit exceeded" (global) not "operation rate limit exceeded".
- Connection sends alternating `primitiveOp` and other messages: both counters are updated independently. A message counts toward the global limit regardless of type; only `primitiveOp` messages count toward the ops limit.

## Failure Handling

- Rate limit check holds the `connState.mu` lock for a brief moment. The lock is uncontended (only one goroutine reads from a WebSocket connection). The lock duration is proportional to the length of `opTimes`, which is bounded by `maxOpsPerSecond = 50`. No performance concern.

## Security Considerations

- The ops rate limit directly limits goroutine accumulation: 50 goroutines/second × 3600 s maximum holdMs = 180,000 goroutines from a single connection before any goroutine terminates. This is still large. The combination of TICKET-010 (connection context cancellation) and the scheduler's 10,000 goroutine cap provides additional protection.
- The 50 ops/s limit may need tuning in production. Consider making it configurable via `Config.MaxOpsPerSecond` in a follow-up.

## Testing Plan

### Unit Tests

In `web/server_test.go`:

```go
func TestOpRateLimitBlocks51st(t *testing.T) {
    ts, cleanup := newTestServer(t)
    defer cleanup()
    conn := dialWS(t, ts.URL+"/ws")
    defer conn.Close()
    drainOne(conn, time.Second) // initial state

    // Create a mutex to operate on
    sendJSON(conn, "createMutex", map[string]interface{}{"id": "m1", "name": "m1"})
    drainOne(conn, time.Second)

    // Send 50 primitiveOp messages rapidly (all within 1 second)
    for i := 0; i < 50; i++ {
        sendJSON(conn, "primitiveOp", map[string]interface{}{
            "id": "m1", "op": "tryLock", "holdMs": 1,
        })
    }

    // Send 51st — should be rate limited
    sendJSON(conn, "primitiveOp", map[string]interface{}{
        "id": "m1", "op": "tryLock", "holdMs": 1,
    })

    // Read 51 responses
    errorSeen := false
    for i := 0; i < 51; i++ {
        _, msg, err := conn.ReadMessage()
        if err != nil {
            break
        }
        if bytes.Contains(msg, []byte("operation rate limit exceeded")) {
            errorSeen = true
        }
    }
    if !errorSeen {
        t.Error("expected 'operation rate limit exceeded' error, got none")
    }
}
```

### Integration Tests

Run `TestWebSocketLoad` which sends 50 connections × 20 cycles each. The load test uses `holdMs: 10` and sends a `lock` + `delete` per cycle, not 50+ ops/s per connection. Verify no rate limit errors occur in the load test.

### E2E Tests

Manual: use a script that sends 100 `primitiveOp` messages in 1 second. Verify that ~50 succeed and ~50 return the ops rate limit error.

## Monitoring Requirements

- `syncprim_op_rate_limit_hits_total` counter on `/metrics`.
- Alert if rate > 10/minute (indicates a misbehaving client or a legitimate use case that needs a higher limit).

## Logging Requirements

When ops rate limit is hit (at `slog.Warn` level, same as global rate limit):
```
level=WARN msg="operation rate limit exceeded" conn_id="<id>" limit=50 window="1s"
```

## Metrics to Track

- `syncprim_op_rate_limit_hits_total` — new counter
- `syncprim_rate_limit_hits_total` — existing counter (rename from whatever it is currently)

## Rollback Plan

Remove `opTimes` from `connState` and the `checkOpRateLimit` call. No data or state impact.

## Acceptance Criteria

- [ ] 51st `primitiveOp` within 1 second receives `"operation rate limit exceeded"` error
- [ ] 200th non-`primitiveOp` message receives `"rate limit exceeded"` error
- [ ] `syncprim_op_rate_limit_hits_total` appears in `/metrics`
- [ ] Constants `maxMsgPerSecond` and `maxOpsPerSecond` defined (not magic numbers)
- [ ] Load test continues to pass without rate limit errors

## Definition of Done

- [ ] Code reviewed and merged
- [ ] Tests passing with race detector
- [ ] Coverage maintained ≥70%
- [ ] Monitoring in place
- [ ] README configuration table updated if limit becomes configurable
- [ ] CHANGELOG entry written
