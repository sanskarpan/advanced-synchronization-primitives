# TICKET-025: Object Pool for Waiter Nodes to Reduce GC Pressure

**Type:** improvement
**Priority:** P2
**Estimate:** M (4 days)
**Epic:** Scalability and Performance
**Labels:** p2, sprint-13, performance, gc, primitives
**Status:** SHIPPED

**Tracking Note:** Implemented on `origin/main`. This ticket is retained as historical planning context; the design notes and checklists below may not match the final shipped implementation verbatim.


## Problem Statement

Every blocking primitive operation (Lock, RLock, Acquire, Wait, etc.) allocates a new `Waiter` struct on the heap via `NewWaiter()`. Under high contention workloads, this creates significant GC pressure:

1. Each `Waiter` allocates a `chan struct{}` (buffered channel, 96 bytes on amd64).
2. Each `Waiter` allocates the struct itself (~80 bytes).
3. Under 200 ops/s per connection × 1000 connections = 200,000 waiters/second allocated.
4. At 200 bytes each: 40 MB/s of allocation, triggering frequent GC cycles.

A `sync.Pool` for `Waiter` nodes eliminates most of these allocations by reusing waiters across operations.

## Context

`NewWaiter()` in `internal/primitives/waiter.go`:
```go
func NewWaiter() *Waiter {
    return &Waiter{
        ch: make(chan struct{}, 1),
    }
}
```

Each call allocates a new struct and a new channel. The channel is the expensive part — `make(chan struct{}, 1)` initializes the channel data structure (hchan) which requires a heap allocation.

Waiters that are "cancelled" (via `waiter.cancelled.Store(true)`) after a CAS-and-enqueue race are never signaled and are simply dropped when the queue is drained. They cannot be safely returned to the pool while still referenced by the `WaiterQueue`.

## Goals

1. Add a `sync.Pool` for `Waiter` allocation.
2. Reset waiter fields before returning to the pool.
3. Ensure that a waiter is only returned to the pool when it is no longer referenced by any `WaiterQueue`.
4. Add benchmarks that measure allocation reduction.
5. Verify with the race detector that pool usage is safe.

## Non-Goals

- Pooling the `WaiterQueue` nodes (the `waiterNode` struct that wraps a `*Waiter`).
- Pooling in non-hot paths (barrier's `Reset()`, etc.).
- Changing the public API of `Waiter` or `WaiterQueue`.

## Technical Design

### Pool Definition

```go
var waiterPool = sync.Pool{
    New: func() interface{} {
        return &Waiter{
            ch: make(chan struct{}, 1),
        }
    },
}

func getWaiter() *Waiter {
    w := waiterPool.Get().(*Waiter)
    // Channel must be drained; it may have been signaled before return.
    select {
    case <-w.ch:
    default:
    }
    // Reset all fields.
    w.cancelled.Store(false)
    return w
}

func putWaiter(w *Waiter) {
    waiterPool.Put(w)
}
```

### When to Put

The waiter can be returned to the pool only after all of the following are true:
1. The waiter has been removed from its `WaiterQueue` (dequeued by `Dequeue` or marked cancelled).
2. No other goroutine holds a pointer to the waiter.

**Safe put locations:**
- In `waiter.Wait()`, after the channel receive returns: `defer putWaiter(waiter)` would be wrong because the caller may still reference `waiter.cancelled`. Instead, the primitive methods that call `NewWaiter` must call `putWaiter` explicitly after they no longer reference the waiter.
- In cancelled fast-path: when `waiter.cancelled.Store(true)` is set (CAS won after enqueue), the waiter is still in the queue but will be skipped by `Dequeue`. It cannot be put back until `Dequeue` removes it. This requires `Dequeue` to put cancelled waiters back into the pool.

### Dequeue Integration

In `WaiterQueue.Dequeue()`, after dequeuing a cancelled waiter, call `putWaiter`:

```go
func (q *WaiterQueue) Dequeue() *Waiter {
    for {
        head := q.head.Load()
        if head == nil {
            return nil
        }
        next := head.next.Load()
        if q.head.CompareAndSwap(head, next) {
            q.size.Add(-1)
            w := head.waiter
            if w.cancelled.Load() {
                putWaiter(w)  // Return cancelled waiter to pool
                continue
            }
            return w
        }
    }
}
```

**Critical invariant:** `putWaiter` is called in `Dequeue` only when the waiter is cancelled (nobody is waiting on `w.ch`). A non-cancelled waiter is returned to the caller; the caller is responsible for eventually calling `putWaiter` after `w.Wait()` returns.

### Caller Responsibility

In each primitive method that calls `getWaiter()`:

```go
waiter := getWaiter()
queue.Enqueue(waiter)

// ... fast path (CAS wins after enqueue) ...
if fastPath {
    // waiter is in the queue but cancelled.
    // Dequeue will call putWaiter when it processes this node.
    // DO NOT call putWaiter here — the waiter is still in the queue.
    waiter.cancelled.Store(true)
    return
}

waiter.Wait()   // blocks until signaled
putWaiter(waiter)  // safe: waiter is dequeued and no longer in queue
```

### ABA Problem Safety

`sync.Pool` objects can be given to different goroutines. A waiter returned to the pool may be retrieved by a different goroutine. The `cancelled` flag reset in `getWaiter()` prevents a new user of the waiter from seeing a stale `cancelled=true` state.

The channel drain in `getWaiter()` prevents a new user from immediately unblocking on a leftover signal.

## Backend Implementation

1. Add `waiterPool` package-level variable to `internal/primitives/waiter.go`.
2. Implement `getWaiter()` and `putWaiter(w *Waiter)`.
3. Update `WaiterQueue.Dequeue()` to call `putWaiter` for cancelled waiters.
4. Update each primitive method to:
   a. Replace `NewWaiter()` with `getWaiter()`.
   b. Call `putWaiter(waiter)` after `waiter.Wait()` returns (in the blocking path).
   c. Do NOT call `putWaiter` in the fast path (CAS wins) — let `Dequeue` handle it.
5. Add `BenchmarkMutexContendedPool` and `BenchmarkSemaphoreBurstPool` to measure allocation reduction.
6. Run `-race -count=10` to stress-test pool interactions.

## Frontend Implementation

None.

## Database / State Changes

None.

## API Changes

None. `NewWaiter()` can be deprecated (unexported function, internal package). No external API changes.

## Infrastructure Requirements

None.

## Edge Cases

- **Waiter signaled between `putWaiter` and `getWaiter`**: The `getWaiter` function drains the channel before returning. A stale signal is discarded. Safe.
- **Two goroutines race on `putWaiter(same_waiter)`**: This must never happen. The invariant is that `putWaiter` is called exactly once per waiter, either by the blocked goroutine after `Wait()` returns, or by `Dequeue` for a cancelled waiter. These are mutually exclusive.
- **Waiter pool size**: `sync.Pool` objects are collected by the GC when under memory pressure. The pool does not grow unboundedly. This is the desired behavior.
- **GC runs between `getWaiter` and `Enqueue`**: The waiter has an active reference (in the goroutine's stack). The GC will not collect it. Safe.

## Failure Handling

None. Pool operations are infallible (`Get` always returns a non-nil value due to the `New` function).

## Security Considerations

None. The waiter pool is an internal implementation detail.

## Testing Plan

### Unit Tests

All existing tests for all 8 primitives serve as correctness tests with the pool. Add:

```go
func TestWaiterPoolResetOnGet(t *testing.T) {
    // Get a waiter, signal it, put it back, get it again
    // Assert channel is empty after second Get
    // Assert cancelled flag is false after second Get
}

func TestWaiterPoolConcurrentSafe(t *testing.T) {
    // 100 goroutines each: get waiter, signal it, put it back
    // Run with -race to detect any races
}
```

### Integration Tests

Run `TestWebSocketLoad` and the `BenchmarkWebSocketConcurrentOps` with and without the pool. Measure allocation reduction.

### E2E Tests

Run the server under load. Profile with `go tool pprof` (heap profile) and verify that `primitives.getWaiter` allocation rate is reduced by 90%+ compared to `NewWaiter`.

## Monitoring Requirements

None. The allocation reduction is observed via profiling and benchmarks.

## Logging Requirements

None.

## Metrics to Track

- `BenchmarkMutexContendedPool allocs/op` should be 0.
- `BenchmarkSemaphoreBurstPool allocs/op` should be significantly reduced.

## Rollback Plan

Replace `getWaiter()` calls with `NewWaiter()` and remove `putWaiter()` calls. The pool variable can remain but will not be used. No behavioral change.

## Acceptance Criteria

- [ ] `BenchmarkMutexContended allocs/op` is 0 (or significantly reduced)
- [ ] All existing primitive tests pass with `-race -count=10`
- [ ] `TestWaiterPoolResetOnGet` verifies channel drain and flag reset
- [ ] No double-free: waiter is returned to pool exactly once
- [ ] No use-after-free: waiter is not accessed after `putWaiter`

## Definition of Done

- [ ] Code reviewed and merged (requires extra careful review given pool safety complexity)
- [ ] Tests passing with `-race -count=10` (not just -count=1)
- [ ] Coverage ≥70%
- [ ] Benchmark results documented in CHANGELOG
- [ ] Architecture improvements document updated
