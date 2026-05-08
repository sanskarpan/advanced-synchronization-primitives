# TICKET-018: Delta WebSocket Updates — Only Broadcast Changed Primitives

**Type:** improvement
**Priority:** P2
**Estimate:** M (4 days)
**Epic:** Scalability and Performance
**Labels:** p2, sprint-13, performance, websocket, bandwidth
**Status:** TODO

## Problem Statement

The server broadcasts the full state of all primitives to all clients every 100 ms via the scheduler's `broadcastUpdates` goroutine. With 100 primitives, each broadcast message can be 50+ KB. With 1000 WebSocket clients, this is 500 MB/s of JSON serialization and network I/O for state that changes infrequently.

In a typical deployment, 90%+ of primitives are idle (not being acquired/released) at any given moment. Broadcasting their unchanged state wastes CPU, memory, and bandwidth.

## Context

In `internal/scheduler/scheduler.go`, `broadcastUpdates` runs every 100 ms:
```go
func (s *Scheduler) broadcastUpdates() {
    ticker := time.NewTicker(100 * time.Millisecond)
    for {
        select {
        case <-ticker.C:
            update := &SchedulerUpdate{
                Primitives: s.GetPrimitives(), // ALL primitives
                Goroutines: s.GetGoroutines(),
                Events:     s.GetEvents(100),
                Metrics:    s.GetMetrics(),
            }
            // broadcast to all connections via updateChan
        }
    }
}
```

`GetPrimitives()` returns a copy of all entries in `s.primitives`. For the client, receiving the full state every 100 ms means replacing its entire local map on each update.

## Goals

1. Add a monotonically increasing version counter to each `PrimitiveInfo`.
2. Track the last version broadcast per client (per-connection, in `HandleWebSocket`).
3. Only include primitives whose version is newer than the last sent version.
4. Include a `generation` sequence number in every update message so clients can detect missed updates and request a full refresh.
5. Provide a `full_refresh` message type that the client can request when its state appears inconsistent.
6. Reduce the size of steady-state broadcasts by ≥80% when most primitives are idle.

## Non-Goals

- Goroutine delta updates (goroutines change frequently; full broadcast is acceptable).
- Per-field delta within a single primitive's stats object.
- Compression (TICKET-019 handles this).

## Technical Design

### Version Counter

Add to `PrimitiveInfo`:
```go
type PrimitiveInfo struct {
    ID           string
    Type         PrimitiveType
    Name         string
    CreatedAt    time.Time
    BlockedCount int32
    Stats        interface{}
    Version      uint64    // monotonically increasing; set on every stat update
}
```

A global sequence: `type versionCounter struct { atomic.Uint64 }; var globalVersion versionCounter`.

Every time `UpdatePrimitiveStats` or `BlockGoroutine`/`UnblockGoroutine` modifies a `PrimitiveInfo`, increment `globalVersion` and set `info.Version = globalVersion.Add(1)`.

### Per-Connection Last-Seen Version

In `HandleWebSocket`, track the last version sent per primitive:
```go
lastSentVersions := make(map[string]uint64) // primitiveID → version
```

In the `forwardSchedulerUpdates` goroutine, when building the outbound `update` message, only include primitives whose `Version > lastSentVersions[id]`:
```go
changed := make(map[string]*PrimitiveInfo)
for id, prim := range update.Primitives {
    if prim.Version > lastSentVersions[id] {
        changed[id] = prim
        lastSentVersions[id] = prim.Version
    }
}
// Also detect deleted primitives: any id in lastSentVersions not in update.Primitives
```

### Deleted Primitive Detection

When a primitive is deleted, it disappears from `GetPrimitives()`. The client must be notified. The server already sends `{"type":"primitiveDeleted","payload":{"id":"..."}}` on deletion. This remains correct.

Also: when building the delta, check for IDs in `lastSentVersions` that are absent from the current `update.Primitives`:
```go
for id := range lastSentVersions {
    if _, exists := update.Primitives[id]; !exists {
        // primitive was deleted since our last broadcast
        delete(lastSentVersions, id)
        // client already received primitiveDeleted message from the delete handler
    }
}
```

### Sequence Number and Full Refresh

Add `sequence` to `SchedulerUpdate`:
```go
type SchedulerUpdate struct {
    Sequence   uint64
    Primitives map[string]*PrimitiveInfo
    // ...
}
```

Add `sequence` to the outbound `update` JSON message. Clients increment their expected sequence and request a full refresh if the sequence gap is > 1.

Client-side full refresh request: `{"type":"requestFullRefresh"}`. Server responds with a `state` message containing all primitives.

## Backend Implementation

1. Add `Version uint64` to `PrimitiveInfo` in `scheduler.go`.
2. Add a package-level `atomic.Uint64` for the global version counter.
3. Increment the version counter in `UpdatePrimitiveStats`, `BlockGoroutine`, `UnblockGoroutine`.
4. Add `Sequence atomic.Uint64` to `Scheduler`.
5. Increment `Sequence` on each `broadcastUpdates` tick.
6. In `HandleWebSocket`'s `forwardSchedulerUpdates`, implement the delta calculation.
7. Add `requestFullRefresh` message handler that sends a full `state` message.
8. Update the outbound `update` message to include `sequence` and only changed primitives.

## Frontend Implementation

1. Update the client-side state update handler to merge delta updates (not replace the entire map):
```javascript
ws.onmessage = function(event) {
    const msg = JSON.parse(event.data);
    if (msg.type === 'update') {
        // Check sequence
        if (msg.payload.sequence !== (lastSequence + 1) && lastSequence !== 0) {
            // Missed updates — request full refresh
            ws.send(JSON.stringify({type: 'requestFullRefresh'}));
        }
        lastSequence = msg.payload.sequence;
        // Merge changed primitives into local state
        Object.assign(primitiveState, msg.payload.primitives);
        renderDashboard();
    } else if (msg.type === 'state') {
        primitiveState = msg.payload.primitives;
        lastSequence = msg.payload.sequence;
        renderDashboard();
    }
};
```

## Database / State Changes

None.

## API Changes

- `update` message payload gains a `sequence` field (non-breaking: old clients ignore it).
- `update` message `primitives` field may be partial (only changed primitives). This IS a breaking change for clients that replace their entire state on each `update`. Old clients that do a full replace will lose track of unchanged primitives. Version update is required.
- New message type: `requestFullRefresh` (client → server).
- New message type: `{"type":"state", ...}` is already defined; it is now also sent in response to `requestFullRefresh`.

## Infrastructure Requirements

None.

## Edge Cases

- First connection: `lastSentVersions` is empty → all primitives are "changed" → send full state.
- Reconnection: client connects fresh → new connection, `lastSentVersions` is empty → full state sent on first update. Client receives `state` message on connect (existing behavior) which initializes its state.
- Very rapid state changes (>10 changes/100ms tick): all changes are captured in the next tick's delta.
- Primitive deleted between ticks: the delete message already handles client notification.

## Failure Handling

- Client sends `requestFullRefresh` repeatedly: apply rate limiting (one per second per connection).

## Security Considerations

None beyond existing considerations.

## Testing Plan

### Unit Tests

```go
func TestDeltaUpdateOnlyChangedPrimitives(t *testing.T) {
    // Create 10 primitives
    // Operate on 1
    // Assert next update message contains only 1 primitive (the changed one)
}

func TestDeltaUpdateFullRefreshOnMissedSequence(t *testing.T) {
    // Connect client, receive some updates
    // Manually send update with sequence gap (simulate missed update)
    // Assert client sends requestFullRefresh
    // Assert server responds with full state message
}
```

### Integration Tests

`TestWebSocketLoad` with delta updates. Verify behavior is identical (clients can reconstruct full state from delta updates).

### E2E Tests

Manual: create 100 primitives, operate 1. Use browser devtools WebSocket inspector to verify that update messages are small.

## Monitoring Requirements

- `syncprim_broadcast_bytes_total` gauge — total bytes broadcast per second. Should drop significantly after this change.

## Logging Requirements

None new.

## Metrics to Track

- `syncprim_broadcast_bytes_total` — to verify bandwidth reduction.
- `syncprim_full_refresh_requests_total` — count of `requestFullRefresh` messages.

## Rollback Plan

Revert the delta calculation logic in `forwardSchedulerUpdates`. Full-state broadcasts resume. Clients receive full state on every tick again.

## Acceptance Criteria

- [ ] Steady-state update messages contain only changed primitives (verified in tests)
- [ ] `sequence` field present in all update messages
- [ ] Client sends `requestFullRefresh` on sequence gap (verified in E2E test)
- [ ] Server responds to `requestFullRefresh` with a full `state` message
- [ ] First connection still receives full state
- [ ] `TestDeltaUpdateOnlyChangedPrimitives` passes
- [ ] Bandwidth in steady state reduced by ≥80% with 100 idle primitives

## Definition of Done

- [ ] Code reviewed and merged
- [ ] Tests passing with race detector
- [ ] Coverage ≥70%
- [ ] README WebSocket protocol table updated
- [ ] CHANGELOG entry (breaking change for `update` message clients)
