# TICKET-009: Graceful WebSocket Connection Draining on Shutdown

**Type:** improvement
**Priority:** P1
**Estimate:** M (3–4 days)
**Epic:** Observability and Reliability
**Labels:** p1, sprint-2, reliability, web-server, shutdown
**Status:** TODO

## Problem Statement

The current shutdown sequence calls `httpServer.Shutdown(ctx)` which stops accepting new HTTP connections and waits up to the context deadline for existing HTTP requests to finish. However, WebSocket connections are long-lived and are not HTTP request/response pairs. The HTTP server's graceful shutdown considers a WebSocket connection "active" until the connection is closed. When `httpServer.Shutdown` times out (after 10 s), it forcibly closes all remaining connections, sending a TCP RST rather than a proper WebSocket close frame.

Clients experience this as WebSocket error code 1006 (abnormal closure — no close frame received). The exponential-backoff reconnection logic triggers immediately.

The correct behavior is:
1. Stop accepting new WebSocket connections.
2. Send a `CloseMessage` with close code 1001 (Going Away) to all active connections.
3. Wait up to 5 s for clients to acknowledge the close.
4. Then call `httpServer.Shutdown` for remaining connections.

WebSocket close code 1001 signals "the endpoint is going away" — browsers understand this and can show a user-friendly "Server is restarting" message instead of triggering error handling.

## Context

In `web/server.go`, the `Shutdown` method:
```go
func (s *Server) Shutdown(ctx context.Context) error {
    s.scheduler.Stop()
    return s.httpServer.Shutdown(ctx)
}
```

There is a connection tracking map (`connCount atomic.Int32`) but no map of active `*websocket.Conn` instances for graceful shutdown.

The `HandleWebSocket` function manages connection lifecycle via goroutines. When `httpServer.Shutdown` is called, the handler's `http.ResponseWriter` is invalidated, causing write errors. There is no ordered shutdown.

## Goals

1. Track all active WebSocket connections in a `sync.Map` (or a `map[*websocket.Conn]struct{}` under a mutex).
2. On `Shutdown`, send close code 1001 to all active connections before calling `httpServer.Shutdown`.
3. Wait up to 5 s for connections to close naturally after sending the close frame.
4. If connections do not close within 5 s, proceed with `httpServer.Shutdown`.
5. Add test `TestGracefulShutdownSendsCloseFrame` that verifies clients receive a 1001 close frame.

## Non-Goals

- Persisting in-flight operation state across restarts (TICKET-005 handles state persistence).
- Draining long-running `primitiveOp` goroutines (TICKET-010 handles this via context cancellation).
- Sending close frames to HTTP (non-WebSocket) connections.

## Technical Design

Add connection tracking to `*Server`:

```go
type Server struct {
    // ... existing fields ...
    activeConns sync.Map // map[*websocket.Conn]struct{}
    draining    atomic.Bool
}
```

In `HandleWebSocket`, register/deregister:
```go
func (s *Server) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
    // ... auth and upgrade ...
    s.activeConns.Store(conn, struct{}{})
    defer s.activeConns.Delete(conn)
    // ... rest of handler ...
}
```

Reject new connections when draining:
```go
if s.draining.Load() {
    http.Error(w, "Server is shutting down", http.StatusServiceUnavailable)
    return
}
```

Add graceful drain in `Shutdown`:
```go
func (s *Server) Shutdown(ctx context.Context) error {
    s.draining.Store(true)
    s.scheduler.Stop()

    // Phase 1: Send close frames to all active connections
    closeMsg := websocket.FormatCloseMessage(websocket.CloseGoingAway, "server shutting down")
    s.activeConns.Range(func(key, _ interface{}) bool {
        conn := key.(*websocket.Conn)
        conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
        conn.WriteMessage(websocket.CloseMessage, closeMsg)
        return true
    })

    // Phase 2: Wait up to 5s for connections to drain
    drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()

    ticker := time.NewTicker(100 * time.Millisecond)
    defer ticker.Stop()

    for {
        select {
        case <-drainCtx.Done():
            // 5s elapsed, proceed with forced shutdown
            goto forceShutdown
        case <-ticker.C:
            remaining := 0
            s.activeConns.Range(func(_, _ interface{}) bool {
                remaining++
                return true
            })
            if remaining == 0 {
                goto forceShutdown
            }
            slog.Info("waiting for WebSocket connections to drain",
                "remaining", remaining)
        }
    }

forceShutdown:
    return s.httpServer.Shutdown(ctx)
}
```

## Backend Implementation

1. Add `activeConns sync.Map` and `draining atomic.Bool` to `*Server`.
2. Update `HandleWebSocket` to reject new connections when `draining.Load() == true`.
3. Update `HandleWebSocket` to register/deregister connections in `activeConns`.
4. Implement the graceful drain logic in `Shutdown` as described above.
5. Add test `TestGracefulShutdownSendsCloseFrame`:
   - Start server.
   - Connect two WebSocket clients.
   - Call `server.Shutdown(ctx)`.
   - Assert both clients receive a close message with code 1001.
   - Assert the server exits within the timeout.
6. Add test `TestNewConnectionsRejectedDuringShutdown`:
   - Start server.
   - Call `server.Shutdown(ctx)`.
   - Attempt a new WebSocket connection.
   - Assert HTTP 503 (service unavailable).

## Frontend Implementation

Update the WebSocket reconnection logic to handle close code 1001 differently:
```javascript
ws.onclose = function(event) {
    if (event.code === 1001) {
        // Server is going away cleanly — show a "Server is restarting" message
        // and delay reconnection by 10 seconds before starting exponential backoff
        statusElement.textContent = 'Server restarting...';
        setTimeout(() => reconnect(), 10000);
    } else {
        // Abnormal closure — use existing exponential backoff
        reconnect();
    }
};
```

## Database / State Changes

Snapshot is saved during shutdown (existing behavior). The graceful drain does not change when the snapshot is saved — it saves after connections are drained.

## API Changes

New HTTP response during shutdown: 503 Service Unavailable for new WebSocket connections.

## Infrastructure Requirements

None.

## Edge Cases

- Connection is added to `activeConns` between the `draining.Store(true)` and the `Range` loop: the `Range` will include it because `Store` happens after the draining check in `HandleWebSocket`. The close message is sent to it.
- Connection is removed from `activeConns` (by its own goroutine) between the `Range` and `conn.WriteMessage`: `conn.WriteMessage` may fail with "use of closed network connection". This is safe — the connection is already closing.
- `sync.Map.Range` iterates a snapshot; concurrent modifications are safe.
- `WriteMessage` on a closing connection: use `recover()` to catch panics from gorilla/websocket on write to closed connection.

## Failure Handling

- If `conn.WriteMessage` fails: log the error at `slog.Debug` level and continue to the next connection. Do not abort the shutdown.
- If the drain timeout expires: log the remaining connection count at `slog.Warn` level, then proceed with `httpServer.Shutdown`.

## Security Considerations

- The close message payload "server shutting down" is informational and does not leak sensitive data.
- The `ServiceUnavailable` response during draining prevents new connections from seeing the server in an inconsistent state.

## Testing Plan

### Unit Tests

```go
func TestGracefulShutdownSendsCloseFrame(t *testing.T) {
    srv := web.NewServerWithConfig(web.Config{AllowedOrigins: []string{"*"}})
    ts := httptest.NewServer(http.HandlerFunc(srv.HandleWebSocket))
    defer ts.Close()

    // Connect two clients
    conn1 := dialWS(t, "ws://"+ts.Listener.Addr().String())
    conn2 := dialWS(t, "ws://"+ts.Listener.Addr().String())
    drainOne(conn1, time.Second) // initial state
    drainOne(conn2, time.Second)

    // Trigger graceful shutdown
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    go srv.Shutdown(ctx)

    // Both clients should receive close code 1001
    for _, conn := range []*websocket.Conn{conn1, conn2} {
        _, _, err := conn.ReadMessage()
        if websocket.IsCloseError(err, websocket.CloseGoingAway) {
            t.Logf("got expected close going away from server")
        } else {
            t.Errorf("expected CloseGoingAway (1001), got: %v", err)
        }
    }
}
```

### Integration Tests

Modify `TestWebSocketLoad` to call `server.Shutdown` while connections are active. Verify no goroutine leaks using `runtime.NumGoroutine()` before and after.

### E2E Tests

Manual: Start server. Open dashboard in browser. Run `kill -TERM <pid>`. Observe browser shows "Server restarting..." (after frontend update). Verify server process exits cleanly.

## Monitoring Requirements

Log connection drain progress:
```
level=INFO msg="starting graceful WebSocket drain" active_connections=47
level=INFO msg="waiting for WebSocket connections to drain" remaining=12
level=INFO msg="WebSocket drain complete" elapsed="1.2s"
level=WARN msg="WebSocket drain timeout; proceeding with forced shutdown" remaining=3
```

## Logging Requirements

As shown in Monitoring Requirements above.

## Metrics to Track

- `syncprim_active_connections` gauge — already tracked via `connCount`. Should reach 0 after successful drain.

## Rollback Plan

Remove the `activeConns` map, the `draining` flag, and the drain loop from `Shutdown`. Restore the original `Shutdown` that only calls `s.scheduler.Stop()` and `s.httpServer.Shutdown(ctx)`. Clients will again receive abrupt 1006 closures on restart. No data corruption.

## Acceptance Criteria

- [ ] Clients receive WebSocket close code 1001 on server shutdown
- [ ] New connections are rejected with 503 after `Shutdown` is called
- [ ] Server waits up to 5 s for connections to drain before forcing shutdown
- [ ] `TestGracefulShutdownSendsCloseFrame` passes
- [ ] `TestNewConnectionsRejectedDuringShutdown` passes
- [ ] All existing tests continue to pass

## Definition of Done

- [ ] Code reviewed and merged
- [ ] Tests passing with race detector
- [ ] Coverage maintained ≥70%
- [ ] CHANGELOG entry written
- [ ] Frontend close code handling updated
