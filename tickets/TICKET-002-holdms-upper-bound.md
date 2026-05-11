# TICKET-002: Increase `holdMs` Upper Bound to 1 Hour and Enforce Maximum

**Type:** security
**Priority:** P0
**Estimate:** S (1 day)
**Epic:** Security and Stability Hardening
**Labels:** security, p0, sprint-1, web-server, primitives
**Status:** SHIPPED

**Tracking Note:** Implemented on `origin/main`. This ticket is retained as historical planning context; the design notes and checklists below may not match the final shipped implementation verbatim.


## Problem Statement

The `holdMs` field in `primitiveOp` messages controls how long the server holds a primitive (lock, acquire, etc.) before auto-releasing it. The current implementation clamps `holdMs` to the range `[1, 5000]` milliseconds. This means the maximum auto-hold is 5 seconds.

This constraint is problematic in two ways:

1. **Too restrictive for legitimate use**: A developer simulating a long-running transaction (e.g., a 30-second database lock) cannot do so. This limits the educational and debugging value of the dashboard.

2. **No explicit maximum enforced as a security boundary**: The 5000 ms cap was chosen arbitrarily and without a stated security rationale. Without a documented and enforced upper bound, future changes could inadvertently remove it.

The correct upper bound should be 1 hour (3,600,000 ms) — long enough for any realistic simulation, but bounded to prevent clients from creating effectively permanent locks.

Additionally, the server currently silently clamps the value without notifying the client. A client requesting `holdMs: 10000` receives a success response as if `holdMs: 5000` was honored. The client has no way to know its value was modified.

## Context

In `web/server.go`, the `primitiveOp` handler contains:
```go
holdMs := payload.HoldMs
if holdMs < 1 {
    holdMs = 100
}
if holdMs > 5000 {
    holdMs = 5000
}
```

The `HoldMs` field is defined in the `primitiveOpPayload` struct:
```go
type primitiveOpPayload struct {
    ID     string `json:"id"`
    Op     string `json:"op"`
    HoldMs int    `json:"holdMs"`
}
```

After clamping, the server spawns a goroutine that calls the appropriate operation and then `time.Sleep(time.Duration(holdMs) * time.Millisecond)` before releasing.

## Goals

1. Raise the maximum `holdMs` from 5,000 ms to 3,600,000 ms (1 hour).
2. Keep the minimum at 1 ms (default to 100 ms when not specified or ≤ 0).
3. When the requested value is clamped, include a `warning` field in the success response.
4. Document the maximum in the README endpoint table and WebSocket protocol table.

## Non-Goals

- Implementing persistent state for holds across server restarts.
- Implementing cancellation of an in-progress hold (separate from connection context cancellation which already works).
- Changing the default `holdMs` from 100 ms.

## Technical Design

Change the clamping constants:
```go
const (
    holdMsMin     = 1
    holdMsMax     = 3_600_000 // 1 hour
    holdMsDefault = 100
)
```

Update the clamping logic:
```go
requestedHoldMs := payload.HoldMs
holdMs := requestedHoldMs
if holdMs < holdMsMin {
    holdMs = holdMsDefault
}
if holdMs > holdMsMax {
    holdMs = holdMsMax
}
```

Update the success response to include a warning when clamping occurs:
```go
type successPayload struct {
    ID      string `json:"id"`
    Op      string `json:"op"`
    Message string `json:"message"`
    Warning string `json:"warning,omitempty"` // new field
}
```

When `requestedHoldMs != holdMs`, set:
```go
warning = fmt.Sprintf("holdMs clamped from %d to %d", requestedHoldMs, holdMs)
```

## Backend Implementation

1. Define `holdMsMin`, `holdMsMax`, `holdMsDefault` constants in `web/server.go`.
2. Capture `requestedHoldMs` before clamping.
3. Add `Warning string` (omitempty) to the success payload struct.
4. Set `Warning` when clamping occurs.
5. Update the success message broadcast to include the warning field.

The goroutine spawned for the operation already uses `time.Sleep`. With a 1-hour maximum, individual goroutines may sleep for up to 1 hour. This is expected and acceptable. The connection context cancellation already ensures the goroutine terminates when the client disconnects (TICKET-010 will enforce this fully via `*Context` methods).

## Frontend Implementation

Update the dashboard tooltip or help text for the `holdMs` input field to indicate the valid range: "Hold duration in milliseconds (1 to 3,600,000). Default: 100."

Update the frontend validation to use the new maximum:
```javascript
const holdMs = parseInt(holdMsInput.value, 10);
if (isNaN(holdMs) || holdMs < 1) {
    holdMsInput.value = 100;
}
if (holdMs > 3600000) {
    showWarning('holdMs capped at 3,600,000 ms (1 hour)');
    holdMsInput.value = 3600000;
}
```

Display the `warning` field from success responses as a non-blocking toast notification (different styling from error toasts — use yellow instead of red).

## Database / State Changes

None. The hold duration affects in-memory goroutine sleep only.

## API Changes

- `holdMs` maximum increased from 5,000 to 3,600,000 in documented range.
- New optional `warning` field in success response payload (omitted when no clamping occurs).
- Existing clients that ignore unknown fields in success responses are unaffected.

## Infrastructure Requirements

None.

## Edge Cases

- `holdMs: 0` → treated as `holdMs: 100` (current behavior preserved).
- `holdMs: -1` → treated as `holdMs: 100` (same as ≤ 0).
- `holdMs: 3600001` → clamped to `3600000`, warning included in response.
- `holdMs: 3600000` → accepted exactly, no warning.
- Floating point `holdMs: 1000.5` → JSON integer deserialization truncates to `1000`. No special handling needed.

## Failure Handling

- If the clamping produces a zero value (which cannot happen with the above logic), default to 100 ms.
- The goroutine sleep is interruptible via the connection context (TICKET-010 will make this explicit).

## Security Considerations

- A 1-hour hold means a single `primitiveOp` with `holdMs: 3600000` will keep a primitive acquired for 1 hour. With the per-connection rate limit of 50 ops/s (TICKET-008), this creates at most 50 held primitives per second per connection, each lasting up to 1 hour. The goroutine count per connection is bounded by the scheduler's 10,000 goroutine cap.
- Operators concerned about long holds should set `Config.MaxConns` to a lower value or implement JWT-based role restrictions that limit `holdMs` for non-admin roles (Phase 6).

## Testing Plan

### Unit Tests

In `web/server_test.go`:

```go
func TestHoldMsDefaultWhenZero(t *testing.T) {
    // Send primitiveOp with holdMs: 0
    // Assert success response received
    // The test cannot easily verify the internal holdMs value,
    // so verify that the operation completes within a reasonable time
}

func TestHoldMsClampingWarning(t *testing.T) {
    // Send createMutex, then primitiveOp lock with holdMs: 9000000
    // Assert success response includes warning field
    // Assert warning mentions clamping
}

func TestHoldMsAtMaximumBoundary(t *testing.T) {
    // Send primitiveOp with holdMs: 3600000
    // Assert success response has no warning field
}
```

### Integration Tests

Verify that the load test (`TestWebSocketLoad`) still passes with `holdMs: 10` (within new limits). The load test already uses `holdMs: 10`.

### E2E Tests

Manual test: enter `holdMs: 9999999` in the dashboard. Verify the frontend shows a warning and clamps the value before sending. Verify the server-side success response includes a warning.

## Monitoring Requirements

No new metrics required. Clamping events are observable via the `warning` field in success responses. If monitoring of clamping frequency is needed, add `syncprim_holdms_clamped_total` counter in a future ticket.

## Logging Requirements

When clamping occurs:
```
level=DEBUG msg="holdMs clamped" requested=9000000 clamped=3600000 id="lock-1" conn_id="<connID>"
```

## Metrics to Track

- No new metrics in this ticket. The `warning` field in responses provides visibility.

## Rollback Plan

Revert the constant changes. The only user-visible change is the reduced maximum from 3,600,000 to 5,000 ms. Clients that relied on values between 5,001 and 3,600,000 ms will have their requests clamped again. No data corruption.

## Acceptance Criteria

- [ ] `holdMs: 3600000` is accepted without clamping
- [ ] `holdMs: 3600001` is clamped to 3,600,000 and success response includes `warning`
- [ ] `holdMs: 0` defaults to 100 ms with no error
- [ ] Frontend validates holdMs range [1, 3600000] before sending
- [ ] README WebSocket protocol table updated with new range
- [ ] Constants defined (not magic numbers) in `web/server.go`

## Definition of Done

- [ ] Code reviewed and merged
- [ ] Tests passing with race detector
- [ ] Coverage maintained ≥70%
- [ ] Documentation updated (README, CHANGELOG)
- [ ] Frontend validation updated
