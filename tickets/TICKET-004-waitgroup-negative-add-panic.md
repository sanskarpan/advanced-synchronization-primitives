# TICKET-004: Improve WaitGroup Negative Counter Panic Message

**Type:** improvement
**Priority:** P0
**Estimate:** S (1 day)
**Epic:** Security and Stability Hardening
**Labels:** p0, sprint-1, primitives, diagnostics
**Status:** SHIPPED

**Tracking Note:** Implemented on `origin/main`. This ticket is retained as historical planning context; the design notes and checklists below may not match the final shipped implementation verbatim.


## Problem Statement

The `WaitGroup.Add` and `WaitGroup.Done` methods panic with the message `"waitgroup: negative counter"` when the counter goes below zero. While the panic correctly prevents undefined behavior (consistent with the standard library's `sync.WaitGroup`), the error message provides insufficient context for debugging. A developer encountering this panic does not know:

1. What the counter value was at the time of the panic.
2. What delta was applied that caused the counter to go negative.
3. Whether it was `Add` or `Done` that triggered the panic.

Compare with the standard library `sync.WaitGroup` which panics with `"sync: negative WaitGroup counter"` — equally terse.

A richer message significantly reduces debugging time in production.

## Context

Current implementation in `internal/primitives/waitgroup.go`:

```go
func (wg *WaitGroup) Add(delta int) {
    n := wg.counter.Add(int32(delta))
    if n < 0 {
        panic("waitgroup: negative counter")
    }
    // ...
}

func (wg *WaitGroup) Done() {
    n := wg.counter.Add(-1)
    if n < 0 {
        panic("waitgroup: negative counter")
    }
    // ...
}
```

The panic message does not include the post-add value `n` or the delta applied.

Note: There is a correctness subtlety here. After `wg.counter.Add(int32(delta))` returns, we know the final value `n`. But we do not know the pre-add value because the counter may have been modified concurrently. The message should include `n` (the resulting counter) and `delta` (what was applied), which together tell the developer the negative is real.

## Goals

1. Improve the panic message in `WaitGroup.Add` to include the resulting counter value and the applied delta.
2. Improve the panic message in `WaitGroup.Done` to indicate it came from `Done` (implicit delta of -1).
3. Add a test `TestWaitGroupNegativeAddPanics` that asserts the panic message format.
4. Add a test `TestWaitGroupNegativeDonePanics` for the Done path.

## Non-Goals

- Changing the behavior (still panics).
- Adding recovery from the panic (callers should fix their logic).
- Changing the panic behavior for negative `delta` passed to `Add` (currently: adds the negative delta, checks if result is < 0; this is the intended behavior per the standard library).

## Technical Design

Use `fmt.Sprintf` to construct the panic string:

```go
func (wg *WaitGroup) Add(delta int) {
    n := wg.counter.Add(int32(delta))
    if n < 0 {
        panic(fmt.Sprintf(
            "waitgroup: negative counter (counter=%d after adding delta=%d)",
            n, delta,
        ))
    }
    // ...
}

func (wg *WaitGroup) Done() {
    n := wg.counter.Add(-1)
    if n < 0 {
        panic(fmt.Sprintf(
            "waitgroup: negative counter (counter=%d after Done; too many Done calls)",
            n,
        ))
    }
    // ...
}
```

The import `"fmt"` is already present in `waitgroup.go`'s package (it's in the same file as the stats String method) — wait, `waitgroup.go` does not currently import `fmt`. Add `"fmt"` to the import block.

## Backend Implementation

1. Add `"fmt"` to the import list in `internal/primitives/waitgroup.go`.
2. Update `Add` panic message as shown above.
3. Update `Done` panic message as shown above.
4. Add tests `TestWaitGroupNegativeAddPanics` and `TestWaitGroupNegativeDonePanics` in `internal/primitives/waitgroup_test.go`.

## Frontend Implementation

None. This is a Go library change.

## Database / State Changes

None.

## API Changes

The panic message format changes. This is technically a behavioral change, but panics are not part of the public API contract. No callers should be relying on the exact panic string. The change is backward-compatible in all practical senses.

## Infrastructure Requirements

None.

## Edge Cases

- `Add(0)`: `counter.Add(0)` returns the current value unchanged. If the counter is already < 0 (impossible in correct usage), this would panic. In practice, this edge case cannot occur if the counter was never negative before.
- `Add(-1)` when counter is 0: `n = 0 + (-1) = -1 < 0` → panic with `"counter=-1 after adding delta=-1"`.
- Large negative delta: `Add(-1000000)` when counter is 5 → `n = -999995` → panic. The message clearly shows the extreme delta.

## Failure Handling

Not applicable — this ticket is about improving panic messages.

## Security Considerations

None. The panic message does not include any user-provided data (IDs, names, keys). It only includes numeric counter values and deltas which are internal state.

## Testing Plan

### Unit Tests

In `internal/primitives/waitgroup_test.go`:

```go
func TestWaitGroupNegativeAddPanics(t *testing.T) {
    wg := NewWaitGroup()
    defer func() {
        r := recover()
        if r == nil {
            t.Fatal("expected panic, got none")
        }
        msg, ok := r.(string)
        if !ok {
            t.Fatalf("expected string panic, got %T: %v", r, r)
        }
        if !strings.Contains(msg, "negative counter") {
            t.Errorf("panic message %q does not mention 'negative counter'", msg)
        }
        if !strings.Contains(msg, "delta=") {
            t.Errorf("panic message %q does not include delta", msg)
        }
        if !strings.Contains(msg, "counter=") {
            t.Errorf("panic message %q does not include counter", msg)
        }
    }()
    wg.Add(-1) // counter was 0, going to -1
}

func TestWaitGroupNegativeDonePanics(t *testing.T) {
    wg := NewWaitGroup()
    defer func() {
        r := recover()
        if r == nil {
            t.Fatal("expected panic, got none")
        }
        msg := fmt.Sprint(r)
        if !strings.Contains(msg, "Done") {
            t.Errorf("panic from Done does not mention 'Done' in message: %q", msg)
        }
    }()
    wg.Done() // counter was 0, going to -1
}

func TestWaitGroupValidUsageNoPanic(t *testing.T) {
    wg := NewWaitGroup()
    wg.Add(3)
    wg.Done()
    wg.Done()
    wg.Done()
    // Should not panic; counter reaches 0 cleanly
}
```

### Integration Tests

The existing `TestWebSocketLoad` exercises `WaitGroup` indirectly. Verify it still passes.

### E2E Tests

Not applicable for a diagnostic improvement.

## Monitoring Requirements

None. Panics are logged by the `recover()` defer in `handlePrimitiveOp`.

## Logging Requirements

When the WebSocket handler recovers a panic from a WaitGroup operation, the improved panic message will appear in the slog error:
```
level=ERROR msg="panic recovered in primitiveOp" panic="waitgroup: negative counter (counter=-1 after Done; too many Done calls)" ...
```

## Metrics to Track

None new for this ticket.

## Rollback Plan

Revert the panic message strings. No functional change; the panic behavior is identical.

## Acceptance Criteria

- [ ] Panic from `Add` with negative result includes `"counter=<N>"` and `"delta=<D>"` in the message
- [ ] Panic from `Done` includes `"Done"` in the message
- [ ] `TestWaitGroupNegativeAddPanics` passes
- [ ] `TestWaitGroupNegativeDonePanics` passes
- [ ] `TestWaitGroupValidUsageNoPanic` passes (regression guard)
- [ ] All existing WaitGroup tests continue to pass

## Definition of Done

- [ ] Code reviewed and merged
- [ ] Tests passing with race detector
- [ ] Coverage maintained ≥70%
- [ ] CHANGELOG entry noting improved diagnostic messages
