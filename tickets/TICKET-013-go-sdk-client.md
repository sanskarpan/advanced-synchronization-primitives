# TICKET-013: Go SDK Client Library for Programmatic Access

**Type:** feature
**Priority:** P2
**Estimate:** L (1–2 weeks)
**Epic:** Developer Experience
**Labels:** p2, sprint-7, sdk, client, developer-experience
**Status:** TODO

## Problem Statement

The only way to interact with the synchronization primitives server programmatically is through raw WebSocket connections with manually constructed JSON messages. There is no typed Go client library. This creates significant friction for:

1. Automated tests that need to verify server behavior.
2. Applications that want to use the server as a distributed lock provider.
3. CI/CD pipelines that need to manage primitives programmatically.
4. The planned CLI tool (TICKET-014) which needs a client to talk to the server.

Without a client library, every integration has to implement WebSocket lifecycle management, message serialization, response correlation, error handling, and reconnection logic independently.

## Context

The server WebSocket protocol is fully documented in the README. Messages are JSON objects with `type` and `payload` fields. Responses are `success`, `error`, `state`, or `update` messages. There is no request ID in the current protocol, which makes response correlation by request ID impossible. The client must correlate by listening for the next message after a send (which is serialized within a connection).

The `internal/loadtest/loadtest_test.go` file contains a primitive ad-hoc client implementation that can serve as a reference.

## Goals

1. Create `pkg/client/client.go` with a `Client` struct.
2. Expose typed methods for all WebSocket operations: create/delete/op for all 8 primitive types.
3. Handle WebSocket connection lifecycle: dial, ping/pong, reconnect with backoff.
4. Handle response correlation: each `Send` blocks until it receives the response for its request.
5. Handle concurrent calls: multiple goroutines may call client methods simultaneously.
6. Provide a context-cancellable API for all operations.
7. Write comprehensive tests using `httptest` and the server's own test infrastructure.

## Non-Goals

- gRPC or REST client (WebSocket is the only transport).
- Supporting server-sent events (the update stream). The client may provide a subscription API for updates in a future ticket.
- Language-specific clients (Python, TypeScript) — Go only in this ticket.
- Supporting server versions other than v0.2.0+.

## Technical Design

### Package Structure

```
pkg/
  client/
    client.go         — Client struct, lifecycle management
    types.go          — request/response types
    primitives.go     — typed methods for each primitive type
    client_test.go    — tests
```

### Client Struct

```go
package client

import (
    "context"
    "encoding/json"
    "sync"
    "time"

    "github.com/gorilla/websocket"
)

type Config struct {
    URL            string        // ws://host:port/ws
    APIKey         string        // Optional Bearer token
    ReconnectDelay time.Duration // Min reconnect delay (default 1s)
    MaxReconnect   time.Duration // Max reconnect delay (default 30s)
    PingInterval   time.Duration // Keepalive interval (default 30s)
}

type Client struct {
    cfg    Config
    conn   *websocket.Conn
    mu     sync.Mutex      // protects conn and send serialization
    // response channel: only one outstanding request at a time
    // (because the server sends responses in order within a connection)
    respCh chan response
    done   chan struct{}
}

func New(cfg Config) *Client
func (c *Client) Connect(ctx context.Context) error
func (c *Client) Close() error
```

### Request/Response Correlation

Because the server sends responses in the order operations are dispatched (within a single goroutine's view), and because each `conn.WriteMessage` is serialized under `c.mu`, we can use a single pending-response channel:

```go
func (c *Client) send(ctx context.Context, msgType string, payload interface{}) (response, error) {
    c.mu.Lock()
    defer c.mu.Unlock()

    msg := message{Type: msgType, Payload: payload}
    data, _ := json.Marshal(msg)
    if err := c.conn.WriteMessage(websocket.TextMessage, data); err != nil {
        return response{}, err
    }

    select {
    case resp := <-c.respCh:
        return resp, nil
    case <-ctx.Done():
        return response{}, ctx.Err()
    }
}
```

A reader goroutine routes all incoming messages to `respCh`.

**Limitation:** This design processes one request at a time per client instance. For concurrent operations, callers should use multiple `Client` instances (one per goroutine). This matches the single-connection-per-goroutine pattern recommended in the load test.

### Primitive Methods

```go
// Mutex operations
func (c *Client) CreateMutex(ctx context.Context, id, name string) error
func (c *Client) LockMutex(ctx context.Context, id string, holdMs int) error
func (c *Client) UnlockMutex(ctx context.Context, id string) error
func (c *Client) TryLockMutex(ctx context.Context, id string) (bool, error)

// RWLock operations
func (c *Client) CreateRWLock(ctx context.Context, id, name string) error
func (c *Client) RLockRWLock(ctx context.Context, id string, holdMs int) error
func (c *Client) RUnlockRWLock(ctx context.Context, id string) error
func (c *Client) LockRWLock(ctx context.Context, id string, holdMs int) error
func (c *Client) UnlockRWLock(ctx context.Context, id string) error

// Semaphore operations
func (c *Client) CreateSemaphore(ctx context.Context, id, name string, capacity int32) error
func (c *Client) AcquireSemaphore(ctx context.Context, id string, holdMs int) error
func (c *Client) ReleaseSemaphore(ctx context.Context, id string) error

// Barrier operations
func (c *Client) CreateBarrier(ctx context.Context, id, name string, parties int32) error
func (c *Client) WaitBarrier(ctx context.Context, id string) error
func (c *Client) BreakBarrier(ctx context.Context, id string) error
func (c *Client) ResetBarrier(ctx context.Context, id string) error

// WaitGroup operations
func (c *Client) CreateWaitGroup(ctx context.Context, id, name string) error
func (c *Client) AddWaitGroup(ctx context.Context, id string) error
func (c *Client) DoneWaitGroup(ctx context.Context, id string) error
func (c *Client) WaitWaitGroup(ctx context.Context, id string) error

// Once operations
func (c *Client) CreateOnce(ctx context.Context, id, name string) error
func (c *Client) DoOnce(ctx context.Context, id string) error
func (c *Client) ResetOnce(ctx context.Context, id string) error

// CondVar operations
func (c *Client) CreateCondVar(ctx context.Context, id, name string) error
func (c *Client) WaitCondVar(ctx context.Context, id string, holdMs int) error
func (c *Client) SignalCondVar(ctx context.Context, id string) error
func (c *Client) BroadcastCondVar(ctx context.Context, id string) error

// Singleflight operations
func (c *Client) CreateSingleflight(ctx context.Context, id, name string) error
func (c *Client) DoSingleflight(ctx context.Context, id string) error

// Generic
func (c *Client) DeletePrimitive(ctx context.Context, id string) error
```

## Backend Implementation

No server-side changes required. The SDK uses the existing WebSocket protocol.

## Frontend Implementation

None.

## Database / State Changes

None.

## API Changes

None to the server. New public package `pkg/client`.

## Infrastructure Requirements

None.

## Edge Cases

- Server sends an `update` or `state` broadcast message while the client is waiting for a `success`/`error` response: the reader goroutine must skip broadcast messages and only route `success`/`error` to `respCh`.
- Server sends an `error` response: `send()` returns an error wrapping the server's error message.
- Connection drops while waiting for response: `conn.ReadMessage()` returns an error; propagate through `respCh`.
- `ctx.Done()` fires before response arrives: `send()` returns `ctx.Err()`. The client may be in an inconsistent state (the server received the request but the client timed out). The client should reconnect before making further calls.

## Failure Handling

- Connection failure: return `ErrNotConnected`. Callers must call `Connect` again.
- Server error response: return `ServerError{Message: "..."}`.
- Context cancellation during operation: return `ctx.Err()`.
- Automatic reconnect: implement in a background goroutine started by `Connect`. Users must enable this explicitly (`cfg.AutoReconnect = true`) to avoid surprise behavior.

## Security Considerations

- The `APIKey` in `Config` is sent as `Authorization: Bearer <key>` header. Do not log the key.
- The client connects over plain WebSocket by default. For production use, the URL should use `wss://` (TLS).

## Testing Plan

### Unit Tests

```go
func TestClientCreateAndDeleteMutex(t *testing.T) {
    ts, cleanup := newTestServer(t)
    defer cleanup()

    c := client.New(client.Config{URL: wsURL(ts)})
    ctx := context.Background()
    require.NoError(t, c.Connect(ctx))
    defer c.Close()

    require.NoError(t, c.CreateMutex(ctx, "m1", "test-mutex"))
    require.NoError(t, c.DeletePrimitive(ctx, "m1"))
}

func TestClientLockUnlock(t *testing.T) {
    // Create mutex, lock with holdMs=50, verify unlock (auto) after 50ms
}

func TestClientContextCancellation(t *testing.T) {
    // Create barrier with parties=2
    // Client A calls WaitBarrier with context that expires in 50ms
    // Assert ErrDeadlineExceeded
}

func TestClientConcurrentOperations(t *testing.T) {
    // 10 client instances, each acquiring/releasing a semaphore
    // Assert no data races, all operations succeed
}
```

### Integration Tests

Run the load test (`TestWebSocketLoad`) rewritten to use the `pkg/client` SDK. Verify behavior is equivalent.

### E2E Tests

Connect the CLI tool (TICKET-014, when available) to a running server. Create primitives, operate them, delete them.

## Monitoring Requirements

None. Client-side metrics are the responsibility of the caller.

## Logging Requirements

Client logs at DEBUG level (disabled by default). Provide a `Logger` interface in `Config` for callers to inject their preferred logger.

## Metrics to Track

None built into the client. Callers can wrap client methods to add their own metrics.

## Rollback Plan

The SDK is an additive package. Removing it does not affect existing functionality.

## Acceptance Criteria

- [ ] `pkg/client` package exists with all primitive methods
- [ ] Client handles `success`/`error` response correlation correctly
- [ ] Client skips `update`/`state` broadcast messages while waiting for a response
- [ ] `TestClientCreateAndDeleteMutex` passes
- [ ] `TestClientConcurrentOperations` passes with `-race`
- [ ] API key authentication supported via `Config.APIKey`
- [ ] godoc comments on all exported types and methods

## Definition of Done

- [ ] Code reviewed and merged
- [ ] Tests passing with race detector
- [ ] Coverage maintained ≥70%
- [ ] godoc complete
- [ ] README updated with SDK usage example
- [ ] CHANGELOG entry written
