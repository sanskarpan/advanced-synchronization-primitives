# TICKET-012: Fair RWLock Variant with FIFO Ordering

**Type:** feature
**Priority:** P2
**Estimate:** L (1–2 weeks)
**Epic:** Advanced Primitive Features
**Labels:** p2, sprint-5, primitives, performance, fairness
**Status:** TODO

## Problem Statement

The current `RWLock` implementation uses writer-preference ordering: once a writer enqueues, all subsequent readers also queue, preventing writer starvation. However, this creates a symmetric problem: under sustained write load, readers may never acquire the lock. The `RWLock` code comments acknowledge this:

> "A continuous stream of writers can starve readers. If your workload has long bursts of writers, consider a reader-preference or fair (FIFO) variant instead."

There is no fair variant provided. Users with mixed read/write workloads where predictable latency for both operations is more important than throughput have no option.

A FIFO-ordered (fair) RWLock grants lock access in strict arrival order. If a reader arrives before a writer, the reader acquires first. If three writers arrive before two readers, the writers acquire in order, then the readers acquire together (since readers can share).

## Context

The `WaiterQueue` in `internal/primitives/waiter.go` is a FIFO queue. A fair RWLock can be implemented by using a single `WaiterQueue` where each node carries a `kind` field (`reader` or `writer`). The release logic dequeues in order:
- If the head is a reader: dequeue all consecutive readers (they can proceed concurrently).
- If the head is a writer: dequeue exactly one writer.

This requires either extending `Waiter` with a kind field or creating a separate `FairWaiter` type.

## Goals

1. Implement `FairRWLock` in `internal/primitives/fair_rwlock.go`.
2. Provide the same API as `RWLock`: `RLock`, `RUnlock`, `Lock`, `Unlock`, `TryRLock`, `TryLock`, `RLockContext`, `LockContext`, `GetStats`.
3. Add comprehensive tests including concurrent reader+writer scenarios.
4. Add benchmarks comparing `FairRWLock` vs `RWLock` under various read/write ratios.
5. Register `FairRWLock` as a primitive type in the WebSocket server (`TypeFairRWLock`).
6. Add `createFairRWLock` message handler.

## Non-Goals

- Replacing the existing `RWLock` (writer-preference variant stays; users choose the variant).
- Implementing the reader-preference variant (TICKET for separate future ticket).
- Proving mathematical fairness properties formally.

## Technical Design

### FairWaiter

Extend the waiter concept with a `kind` discriminator:

```go
type waiterKind uint8

const (
    waiterKindReader waiterKind = iota
    waiterKindWriter
)

type fairWaiter struct {
    Waiter        // embed existing Waiter
    kind waiterKind
}
```

### FairWaiterQueue

A linked queue of `fairWaiter` nodes. The `Dequeue` method returns the next non-cancelled waiter.

A `DequeueReaders` method dequeues all consecutive reader waiters at the head of the queue, returning them as a slice.

### FairRWLock State Machine

State: `atomic.Int32` encoding:
- Bits 0–29: active reader count
- Bit 30: writer active flag
- No writer waiting bit needed (the queue handles ordering)

```
type FairRWLock struct {
    state   atomic.Int32
    queue   *FairWaiterQueue
    // metrics (same as RWLock)
    readerAcquires atomic.Int64
    writerAcquires atomic.Int64
    // ...
    createdAt time.Time
}
```

### RLock (Fair)

```
1. If state == 0 (no writer, no readers) AND queue is empty:
   CAS(0, 1) → acquire immediately
2. Else if no writer in queue and no writer active and queue head is not a writer:
   CAS(state, state+1) → join existing readers
3. Else:
   Enqueue as reader
   Re-check (lost-wakeup fix)
   Wait
```

### Lock (Fair, Writer)

```
1. If state == 0 AND queue is empty:
   CAS(0, WriterBit) → acquire immediately
2. Else:
   Enqueue as writer
   Re-check (lost-wakeup fix)
   Wait
```

### Unlock (Fair, Writer)

```
1. Clear WriterBit
2. Peek queue head:
   - If head is reader: DequeueReaders() → wake all consecutive readers
   - If head is writer: Dequeue() → wake one writer
   - If empty: no wakeup
```

### RUnlock (Fair, Reader)

```
1. Decrement reader count
2. If reader count reaches 0:
   Peek queue head:
   - If head is writer: Dequeue() → wake one writer
   - If head is reader: DequeueReaders() → wake all (shouldn't happen with fair policy but handle gracefully)
   - If empty: no wakeup
```

## Backend Implementation

1. Create `internal/primitives/fair_waiter.go` with `waiterKind` and `fairWaiter`.
2. Create `internal/primitives/fair_waiter_queue.go` with `FairWaiterQueue` (similar to `WaiterQueue` but with typed nodes).
3. Create `internal/primitives/fair_rwlock.go` with `FairRWLock`.
4. Add `TypeFairRWLock PrimitiveType = "FairRWLock"` to `scheduler.go`.
5. Add `createFairRWLock` handler in `web/server.go` (identical to `createRWLock` but uses `FairRWLock`).
6. Add operations `fairrlock`, `fairrunlock`, `fairlock`, `fairunlock`, `tryfairrlock`, `tryfairlock` to the `primitiveOp` switch — or reuse existing `rlock`/`runlock`/`lock`/`unlock` switch branches by detecting the primitive type.
7. Add `FairRWLockStats` struct (same fields as `RWLockStats` plus a `Policy string` field set to `"fair-fifo"`).

## Frontend Implementation

1. Add `FairRWLock` to the primitive type selector in the create panel.
2. Reuse the `RWLock` operation buttons (rlock, runlock, lock, unlock).
3. Display `Policy: fair-fifo` in the stats panel.
4. Use a distinct color in the canvas visualization to differentiate from standard `RWLock`.

## Database / State Changes

Snapshot persistence: add `"fair_rwlock"` as a valid `type` value in `snapshotPrimitive`. On restore, call `primitives.NewFairRWLock()`.

## API Changes

New create message type: `createFairRWLock` with payload `{id, name}`.

New primitive type in scheduler: `"FairRWLock"`.

Operations are the same as `RWLock` (the `primitiveOp` handler dispatches by the underlying type).

## Infrastructure Requirements

None.

## Edge Cases

- All readers, no writers: FIFO order means readers are acquired in the order they arrived. Concurrent readers still share the lock simultaneously once they reach the head.
- Reader then writer then reader: the first reader acquires. The writer waits. The second reader queues behind the writer (FIFO). When the first reader releases, the writer acquires. When the writer releases, the second reader acquires.
- Many concurrent goroutines calling `RLock` simultaneously while empty: CAS race on `state`. Only one wins the CAS. Others enqueue. The winner does not need to dequeue the others — the "wake consecutive readers" logic in `Unlock` handles that.
- `TryRLock` / `TryLock`: these are non-blocking CAS attempts. They succeed only if the lock is free and the queue is empty (strict fairness: if someone is waiting, non-blocking attempts fail to avoid cutting in line).

## Failure Handling

Same panic conditions as `RWLock`: `RUnlock` when not locked, `Unlock` when no writer active.

## Security Considerations

None beyond the general considerations for all primitives.

## Testing Plan

### Unit Tests

```go
func TestFairRWLockFIFOOrdering(t *testing.T) {
    // Start goroutines in a controlled order:
    // 1. Reader A acquires
    // 2. Writer W queues (blocked by Reader A)
    // 3. Reader B queues (must wait for W per FIFO, unlike writer-preference RWLock)
    // 4. Reader A releases
    // 5. Verify W acquires next (not Reader B)
    // 6. W releases
    // 7. Verify B acquires
}

func TestFairRWLockNoStarvation(t *testing.T) {
    // 10 goroutines: alternating readers and writers enqueue
    // Verify all eventually acquire and release
    // No goroutine waits more than N*2 turns
}

func TestFairRWLockRaceDetector(t *testing.T) {
    // -race test with 100 concurrent readers + 10 writers
}
```

### Integration Tests

Add `createFairRWLock` and operations to the WebSocket server tests.

### E2E Tests

Manual dashboard test: create a FairRWLock, acquire a read lock, attempt a write lock from a second tab (it queues), attempt another read lock (it should queue BEHIND the write lock, unlike standard RWLock).

## Monitoring Requirements

Same metrics as `RWLock`: acquires, waits, wait time histogram, current holders.

## Logging Requirements

None beyond existing primitive lifecycle logging.

## Metrics to Track

- All existing `syncprim_*` metrics apply, with `type="FairRWLock"` label.

## Rollback Plan

Remove `fair_rwlock.go`, `fair_waiter.go`, `fair_waiter_queue.go`, and the `createFairRWLock` handler. No impact on existing `RWLock` users.

## Acceptance Criteria

- [ ] `FairRWLock` provides FIFO ordering (readers cannot cut in front of a waiting writer)
- [ ] `TestFairRWLockFIFOOrdering` passes deterministically
- [ ] `TestFairRWLockRaceDetector` passes with `-race`
- [ ] `createFairRWLock` WebSocket message works end-to-end
- [ ] Dashboard shows `FairRWLock` with correct operations
- [ ] Benchmarks added and results documented

## Definition of Done

- [ ] Code reviewed and merged
- [ ] Tests passing with race detector (`-count=3`)
- [ ] Coverage maintained ≥70%
- [ ] Benchmarks added
- [ ] Documentation updated (README primitive table, WebSocket message table)
- [ ] CHANGELOG entry written
