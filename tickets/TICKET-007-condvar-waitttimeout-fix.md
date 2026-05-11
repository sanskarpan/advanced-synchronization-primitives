# TICKET-007: Fix CondVar.WaitTimeout Elapsed Time Tracking

**Type:** bug
**Priority:** P1
**Estimate:** S (1 day)
**Epic:** Observability and Reliability
**Labels:** p1, sprint-2, bug, primitives, metrics, observability
**Status:** SHIPPED

**Tracking Note:** Implemented on `origin/main`. This ticket is retained as historical planning context; the design notes and checklists below may not match the final shipped implementation verbatim.


## Problem Statement

`CondVar.WaitTimeout` does not record elapsed wait time into `totalWaitTime` when the wait times out. The timeout path sets `waiter.cancelled.Store(true)` but does not call `cv.totalWaitTime.Add(waitTime)`.

This causes `CondVarStats.TotalWaitTimeNs` to be systematically undercounted in any workload where timeouts are frequent. Downstream effects:

1. The average wait time calculation (`TotalWaitTimeNs / Waits`) underestimates actual average wait times.
2. The Prometheus histogram for CondVar wait times is not populated on the timeout path.
3. Operators cannot distinguish between "waits are fast" and "waits are timing out frequently" from the metrics alone.

## Context

In `internal/primitives/condvar.go`, `CondVar.WaitTimeout`:

```go
func (cv *CondVar) WaitTimeout(m *Mutex, timeout time.Duration) bool {
    // ...
    startTime := time.Now()
    // ...
    signaled := waiter.WaitTimeout(timeout)

    if signaled {
        waitTime := time.Since(startTime).Nanoseconds()
        cv.totalWaitTime.Add(waitTime)   // BUG: only updated on signal, not on timeout
    } else {
        // Timed out: cancel the waiter so a future Signal/Broadcast skip it.
        waiter.cancelled.Store(true)
        // BUG: totalWaitTime is NOT updated here
    }

    // Re-acquire the mutex
    m.Lock()

    return signaled
}
```

Compare with `Semaphore.AcquireNTimeout` which correctly records `totalWaitTime` unconditionally after `waiter.WaitTimeout` returns.

Also note: `cv.waits` is incremented at the top of `WaitTimeout`. If `totalWaitTime` is only updated for signals, then `avgWait = TotalWaitTimeNs / Waits` is computed across all waits but the total only includes signaled waits — a mixed-denominator average that is always an underestimate.

## Goals

1. Record `totalWaitTime` regardless of whether `WaitTimeout` returned `true` (signaled) or `false` (timed out).
2. Record the elapsed time including the mutex re-acquisition after the wait.
3. Add a test `TestCondVarWaitTimeoutElapsedTracking` that verifies `stats.TotalWaitTimeNs > 0` after a timed-out wait.
4. Add a test that verifies `TotalWaitTimeNs` is accumulated on both signaled and timed-out paths.

## Non-Goals

- Changing the return semantics of `WaitTimeout` (still returns `true` for signaled, `false` for timed out).
- Adding a separate `TimeoutWaitTimeNs` counter (the total should include all waits).
- Changing `CondVar.Wait` (it already records unconditionally via `totalWaitTime.Add(waitTime)` in the non-conditional block).

## Technical Design

Move the elapsed time recording outside the `if signaled` block:

```go
func (cv *CondVar) WaitTimeout(m *Mutex, timeout time.Duration) bool {
    if !m.IsLocked() {
        panic("condvar: WaitTimeout called on unlocked mutex")
    }

    startTime := time.Now()

    waiter := NewWaiter()
    cv.waiters.Enqueue(waiter)
    cv.waits.Add(1)

    // Unlock the mutex
    m.Unlock()

    // Wait for signal or timeout
    signaled := waiter.WaitTimeout(timeout)

    if !signaled {
        // Timed out: cancel the waiter so a future Signal/Broadcast skips it.
        waiter.cancelled.Store(true)
    }

    // Re-acquire the mutex
    m.Lock()

    // Record elapsed wait time unconditionally.
    // Use time.Since(startTime) AFTER m.Lock() to include mutex re-acquisition
    // latency in the measurement, matching CondVar.Wait behavior.
    waitTime := time.Since(startTime).Nanoseconds()
    cv.totalWaitTime.Add(waitTime)

    return signaled
}
```

**Note on measurement point:** The current `Wait` method records `waitTime` before calling `m.Lock()`:
```go
waiter.Wait()
waitTime := time.Since(startTime).Nanoseconds()
cv.totalWaitTime.Add(waitTime)
m.Lock()
```

For consistency, `WaitTimeout` should also record before `m.Lock()`. However, both choices are defensible. The ticket adopts "before m.Lock" for consistency with `Wait`.

Revised design (consistent with `Wait`):

```go
    signaled := waiter.WaitTimeout(timeout)

    if !signaled {
        waiter.cancelled.Store(true)
    }

    // Record elapsed time BEFORE re-acquiring the mutex,
    // consistent with CondVar.Wait measurement convention.
    waitTime := time.Since(startTime).Nanoseconds()
    cv.totalWaitTime.Add(waitTime)

    m.Lock()

    return signaled
}
```

## Backend Implementation

1. Edit `condvar.go` `WaitTimeout` as described above.
2. Add tests in `condvar_test.go`:
   - `TestCondVarWaitTimeoutElapsedTracking`: Start a goroutine that calls `WaitTimeout(mu, 10*time.Millisecond)`. No signal sent. Assert `cv.GetStats().TotalWaitTimeNs >= 10_000_000` (10 ms in nanoseconds) after the goroutine returns.
   - `TestCondVarWaitTimeoutBothPathsAccumulate`: Two goroutines — one times out, one is signaled. Assert `TotalWaitTimeNs > 0` in both cases independently, then combined.
3. Verify existing tests in `condvar_test.go` still pass (the change should not break them).

## Frontend Implementation

None. This is a Go library metrics fix.

## Database / State Changes

None.

## API Changes

None. The return value of `WaitTimeout` is unchanged.

## Infrastructure Requirements

None.

## Edge Cases

- `timeout = 0`: `waiter.WaitTimeout(0)` returns immediately (`false`). `time.Since(startTime)` will be approximately 0 ns. `totalWaitTime.Add(0)` is a no-op. This is correct.
- `timeout > 0` but signal arrives before timeout: `signaled = true`. `totalWaitTime` is still updated (this was already working). No behavioral change.
- Concurrent `WaitTimeout` calls: Each goroutine has its own `startTime` and `waitTime`. The atomic `Add` on `totalWaitTime` is safe for concurrent updates.

## Failure Handling

Not applicable — this is a metrics correctness fix with no failure modes.

## Security Considerations

None.

## Testing Plan

### Unit Tests

```go
func TestCondVarWaitTimeoutElapsedTracking(t *testing.T) {
    mu := NewMutex()
    cv := NewCondVar()

    mu.Lock()
    // Timeout after 20ms, no signal sent
    signaled := cv.WaitTimeout(mu, 20*time.Millisecond)
    mu.Unlock()

    if signaled {
        t.Fatal("expected timeout, got signal")
    }

    stats := cv.GetStats()
    if stats.TotalWaitTimeNs < 20_000_000 {
        t.Errorf("TotalWaitTimeNs = %d, expected >= 20ms (%d ns)",
            stats.TotalWaitTimeNs, 20_000_000)
    }
    if stats.Waits != 1 {
        t.Errorf("Waits = %d, expected 1", stats.Waits)
    }
}

func TestCondVarWaitTimeoutSignaledPathStillRecords(t *testing.T) {
    mu := NewMutex()
    cv := NewCondVar()

    var wg sync.WaitGroup
    wg.Add(1)
    go func() {
        defer wg.Done()
        time.Sleep(5 * time.Millisecond)
        cv.Signal()
    }()

    mu.Lock()
    signaled := cv.WaitTimeout(mu, time.Second)
    mu.Unlock()

    wg.Wait()

    if !signaled {
        t.Fatal("expected signal, got timeout")
    }
    stats := cv.GetStats()
    if stats.TotalWaitTimeNs == 0 {
        t.Error("TotalWaitTimeNs should be > 0 after signaled wait")
    }
}
```

### Integration Tests

Run the full test suite with race detector. Verify the CondVar-related tests in `web/server_test.go` pass.

### E2E Tests

Manual: Create a CondVar primitive via the dashboard. Perform a `wait` operation with a short timeout. Verify the `/metrics` endpoint shows non-zero wait time for the CondVar.

## Monitoring Requirements

After this fix, the Prometheus histogram for CondVar wait duration will include timeout observations. If dashboards were built assuming the histogram only counted signaled waits, they may need updating.

## Logging Requirements

None. This is a metrics counter fix.

## Metrics to Track

- `syncprim_wait_duration_seconds` histogram for CondVar — should now correctly populate the timeout bucket.
- `syncprim_waits_total` for CondVar — already counts timeouts (unchanged).

## Rollback Plan

Revert the `WaitTimeout` change. `TotalWaitTimeNs` will undercount again. No functional impact — this is a metrics-only fix.

## Acceptance Criteria

- [ ] `TestCondVarWaitTimeoutElapsedTracking` passes and asserts `TotalWaitTimeNs >= timeout_duration`
- [ ] `TestCondVarWaitTimeoutSignaledPathStillRecords` passes
- [ ] `totalWaitTime` is recorded outside the `if signaled` block
- [ ] All existing CondVar tests pass
- [ ] No race detector findings

## Definition of Done

- [ ] Code reviewed and merged
- [ ] Tests passing with race detector
- [ ] Coverage maintained ≥70%
- [ ] CHANGELOG entry: "Fix: CondVar.WaitTimeout now correctly records elapsed time on timeout path"
