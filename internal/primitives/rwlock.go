package primitives

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Compile-time check: *RWLock implements sync.Locker (write lock interface).
var _ sync.Locker = (*RWLock)(nil)

const (
	rwLockMaxReaders = 1<<30 - 1
	rwLockWriterBit  = 1 << 30
)

// RWLock is a read-write lock implemented from atomic operations.
// Multiple readers can hold the lock simultaneously; only one writer can
// hold the lock exclusively.
//
// Writer preference: once a writer is queued in writerQueue, new readers
// will also queue in readerQueue rather than acquiring immediately.  This
// prevents writer starvation but means that a continuous stream of writers
// can starve readers.  If your workload has long bursts of writers, consider
// a reader-preference or fair (FIFO) variant instead.
type RWLock struct {
	// state encoding:
	// - bits 0-29: reader count
	// - bit 30: writer active flag
	// - bit 31: writer waiting flag
	state atomic.Int32

	// Queues for waiting readers and writers
	readerQueue *WaiterQueue
	writerQueue *WaiterQueue

	// Metrics
	readerAcquires atomic.Int64
	writerAcquires atomic.Int64
	readerWaits    atomic.Int64
	writerWaits    atomic.Int64
	totalWaitTime  atomic.Int64 // nanoseconds
	currentReaders atomic.Int32
	writerHeld     atomic.Int32
	createdAt      time.Time
}

// NewRWLock creates a new RWLock
func NewRWLock() *RWLock {
	return &RWLock{
		readerQueue: NewWaiterQueue(),
		writerQueue: NewWaiterQueue(),
		createdAt:   time.Now(),
	}
}

// RLock acquires a read lock.
//
// Writer preference: if a writer is active or queued (writerQueue non-empty),
// this reader will block even if no write lock is currently held.
// This prevents new readers from perpetually delaying waiting writers, but
// creates a potential reader-starvation scenario under heavy write load.
//
// Lost-wakeup fix: after enqueuing a waiter we re-check the state; if the
// writer has released (or there is no writer) we grab the read slot directly
// and cancel the phantom waiter.
func (rw *RWLock) RLock() {
	startTime := time.Now()

	for {
		state := rw.state.Load()

		// Check if writer is active or waiting
		if state&rwLockWriterBit != 0 || !rw.writerQueue.IsEmpty() {
			// Need to wait
			waiter := NewWaiter()
			rw.readerQueue.Enqueue(waiter)
			rw.readerWaits.Add(1)

			// Re-check after enqueue to close the lost-wakeup window.
			// If there is no longer a writer, try to grab the read slot.
			st := rw.state.Load()
			if st&rwLockWriterBit == 0 && rw.writerQueue.IsEmpty() {
				if st < rwLockMaxReaders && rw.state.CompareAndSwap(st, st+1) {
					waiter.cancelled.Store(true)
					rw.currentReaders.Add(1)
					rw.readerAcquires.Add(1)
					return
				}
			}

			waiter.Wait()

			waitTime := time.Since(startTime).Nanoseconds()
			rw.totalWaitTime.Add(waitTime)
			continue
		}

		// Check reader count overflow
		if state >= rwLockMaxReaders {
			panic("rwlock: too many readers")
		}

		// Try to increment reader count
		if rw.state.CompareAndSwap(state, state+1) {
			rw.currentReaders.Add(1)
			rw.readerAcquires.Add(1)
			return
		}
	}
}

// RUnlock releases a read lock
func (rw *RWLock) RUnlock() {
	for {
		state := rw.state.Load()

		readerCount := state & rwLockMaxReaders
		if readerCount == 0 {
			panic("rwlock: RUnlock of unlocked RWLock")
		}

		// Decrement reader count
		newState := state - 1
		if rw.state.CompareAndSwap(state, newState) {
			rw.currentReaders.Add(-1)

			// If this was the last reader, hand off appropriately.
			// Use structural Dequeue (not IsEmpty): IsEmpty is size-based and
			// can be non-zero due to cancelled phantom waiters, which would
			// skip wakeAllReaders and permanently strand waiting readers.
			if newState&rwLockMaxReaders == 0 {
				writer := rw.writerQueue.Dequeue()
				if writer != nil {
					writer.Signal()
				} else {
					rw.wakeAllReaders()
				}
			}

			return
		}
	}
}

// Lock acquires a write lock.
//
// Lost-wakeup fix: after enqueuing a waiter we re-check state; if state is
// now 0 we acquire and cancel the phantom waiter.
func (rw *RWLock) Lock() {
	startTime := time.Now()

	for {
		state := rw.state.Load()

		// Check if any readers or writer active
		if state != 0 {
			// Need to wait
			waiter := NewWaiter()
			rw.writerQueue.Enqueue(waiter)
			rw.writerWaits.Add(1)

			// Re-check after enqueue to close the lost-wakeup window.
			if rw.state.CompareAndSwap(0, rwLockWriterBit) {
				waiter.cancelled.Store(true)
				rw.writerHeld.Store(1)
				rw.writerAcquires.Add(1)
				return
			}

			waiter.Wait()

			waitTime := time.Since(startTime).Nanoseconds()
			rw.totalWaitTime.Add(waitTime)
			continue
		}

		// Try to acquire write lock
		if rw.state.CompareAndSwap(0, rwLockWriterBit) {
			rw.writerHeld.Store(1)
			rw.writerAcquires.Add(1)
			return
		}
	}
}

// Unlock releases a write lock
func (rw *RWLock) Unlock() {
	state := rw.state.Load()

	if state&rwLockWriterBit == 0 {
		panic("rwlock: Unlock of unlocked RWLock")
	}

	// Clear writer bit
	if !rw.state.CompareAndSwap(state, 0) {
		panic("rwlock: inconsistent state in Unlock")
	}

	rw.writerHeld.Store(0)

	// Wake waiting goroutines.
	// Use structural Dequeue (not IsEmpty) so that a phantom cancelled writer
	// waiter does not block the fallthrough to wakeAllReaders.
	writer := rw.writerQueue.Dequeue()
	if writer != nil {
		writer.Signal()
	} else {
		rw.wakeAllReaders()
	}
}

// RLockContext acquires a read lock or returns ctx.Err() if the context
// is cancelled before the lock can be acquired.
func (rw *RWLock) RLockContext(ctx context.Context) error {
	startTime := time.Now()

	for {
		state := rw.state.Load()

		if state&rwLockWriterBit == 0 && rw.writerQueue.IsEmpty() {
			if state < rwLockMaxReaders {
				if rw.state.CompareAndSwap(state, state+1) {
					rw.currentReaders.Add(1)
					rw.readerAcquires.Add(1)
					return nil
				}
				continue
			}
		}

		// Check context before blocking.
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		waiter := NewWaiter()
		rw.readerQueue.Enqueue(waiter)
		rw.readerWaits.Add(1)

		// Re-check after enqueue.
		st := rw.state.Load()
		if st&rwLockWriterBit == 0 && rw.writerQueue.IsEmpty() {
			if st < rwLockMaxReaders && rw.state.CompareAndSwap(st, st+1) {
				waiter.cancelled.Store(true)
				rw.currentReaders.Add(1)
				rw.readerAcquires.Add(1)
				return nil
			}
		}

		if err := waiter.WaitContext(ctx); err != nil {
			return err
		}

		waitTime := time.Since(startTime).Nanoseconds()
		rw.totalWaitTime.Add(waitTime)
	}
}

// LockContext acquires a write lock or returns ctx.Err() if the context
// is cancelled before the lock can be acquired.
func (rw *RWLock) LockContext(ctx context.Context) error {
	startTime := time.Now()

	for {
		state := rw.state.Load()

		if state == 0 {
			if rw.state.CompareAndSwap(0, rwLockWriterBit) {
				rw.writerHeld.Store(1)
				rw.writerAcquires.Add(1)
				return nil
			}
			continue
		}

		// Check context before blocking.
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		waiter := NewWaiter()
		rw.writerQueue.Enqueue(waiter)
		rw.writerWaits.Add(1)

		// Re-check after enqueue.
		if rw.state.CompareAndSwap(0, rwLockWriterBit) {
			waiter.cancelled.Store(true)
			rw.writerHeld.Store(1)
			rw.writerAcquires.Add(1)
			return nil
		}

		if err := waiter.WaitContext(ctx); err != nil {
			return err
		}

		waitTime := time.Since(startTime).Nanoseconds()
		rw.totalWaitTime.Add(waitTime)
	}
}

// TryRLock tries to acquire a read lock without blocking.
// IsEmpty is used here as a conservative fast-path: it may briefly report
// non-empty due to size-counter lag, causing an unnecessary false-return.
// This is safe — it errs on the side of caution (not acquiring the lock),
// not on the side of incorrectly granting it.
func (rw *RWLock) TryRLock() bool {
	for {
		state := rw.state.Load()

		// Check if writer is active or waiting.
		// IsEmpty is a conservative fast-path (see doc comment above).
		if state&rwLockWriterBit != 0 || !rw.writerQueue.IsEmpty() {
			return false
		}

		// Check reader count overflow
		if state >= rwLockMaxReaders {
			return false
		}

		// Try to increment reader count
		if rw.state.CompareAndSwap(state, state+1) {
			rw.currentReaders.Add(1)
			rw.readerAcquires.Add(1)
			return true
		}
	}
}

// TryLock tries to acquire a write lock without blocking.
// The CAS on state==0 ensures correctness; no IsEmpty call is needed here.
func (rw *RWLock) TryLock() bool {
	if rw.state.CompareAndSwap(0, rwLockWriterBit) {
		rw.writerHeld.Store(1)
		rw.writerAcquires.Add(1)
		return true
	}
	return false
}

// wakeAllReaders wakes up all waiting readers.
// Uses structural drain (Dequeue until nil) instead of IsEmpty to avoid
// the size-counter lag described in barrier.wakeAll.
// WaiterQueue.Dequeue already skips cancelled entries.
func (rw *RWLock) wakeAllReaders() {
	for {
		waiter := rw.readerQueue.Dequeue()
		if waiter == nil {
			return
		}
		waiter.Signal()
	}
}

// GetStats returns statistics about the RWLock
func (rw *RWLock) GetStats() RWLockStats {
	return RWLockStats{
		ReaderAcquires: rw.readerAcquires.Load(),
		WriterAcquires: rw.writerAcquires.Load(),
		ReaderWaits:    rw.readerWaits.Load(),
		WriterWaits:    rw.writerWaits.Load(),
		TotalWaitTimeNs: rw.totalWaitTime.Load(),
		CurrentReaders: rw.currentReaders.Load(),
		WriterHeld:     rw.writerHeld.Load() == 1,
		ReadersWaiting: rw.readerQueue.Size(),
		WritersWaiting: rw.writerQueue.Size(),
		Age:            time.Since(rw.createdAt),
	}
}

// RWLockStats contains statistics about an RWLock
type RWLockStats struct {
	ReaderAcquires  int64
	WriterAcquires  int64
	ReaderWaits     int64
	WriterWaits     int64
	TotalWaitTimeNs int64
	CurrentReaders  int32
	WriterHeld      bool
	ReadersWaiting  int64
	WritersWaiting  int64
	Age             time.Duration
}

// String returns a string representation of the stats
func (s RWLockStats) String() string {
	totalWaits := s.ReaderWaits + s.WriterWaits
	avgWaitNs := int64(0)
	if totalWaits > 0 {
		avgWaitNs = s.TotalWaitTimeNs / totalWaits
	}
	return fmt.Sprintf(
		"RWLock Stats:\n"+
			"  Reader Acquires: %d\n"+
			"  Writer Acquires: %d\n"+
			"  Reader Waits: %d\n"+
			"  Writer Waits: %d\n"+
			"  Avg Wait Time: %v\n"+
			"  Current Readers: %d\n"+
			"  Writer Held: %v\n"+
			"  Readers Waiting: %d\n"+
			"  Writers Waiting: %d\n"+
			"  Age: %v",
		s.ReaderAcquires,
		s.WriterAcquires,
		s.ReaderWaits,
		s.WriterWaits,
		time.Duration(avgWaitNs),
		s.CurrentReaders,
		s.WriterHeld,
		s.ReadersWaiting,
		s.WritersWaiting,
		s.Age,
	)
}

