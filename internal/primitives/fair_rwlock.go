package primitives

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Compile-time check: *FairRWLock implements sync.Locker (write lock interface).
var _ sync.Locker = (*FairRWLock)(nil)

// FairRWLock is a strict FIFO read-write lock.
//
// Fairness policy:
// - Readers and writers enqueue into a single FIFO queue.
// - The queue head determines who acquires next.
// - Consecutive readers at the head are woken together.
//
// This policy avoids starvation on either side at the cost of lower peak
// throughput than writer-preference RWLock under some workloads.
type FairRWLock struct {
	// state encoding:
	// - bits 0-29: reader count
	// - bit 30: writer active flag
	state atomic.Int32

	queue *fairWaiterQueue

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

// FairRWLockStats contains statistics about a fair RW lock.
type FairRWLockStats struct {
	Policy          string
	ReaderAcquires  int64
	WriterAcquires  int64
	ReaderWaits     int64
	WriterWaits     int64
	TotalWaitTimeNs int64
	CurrentReaders  int32
	WriterHeld      bool
	WaitersQueued   int64
	Age             time.Duration
}

// NewFairRWLock creates a new FIFO-ordered RW lock.
func NewFairRWLock() *FairRWLock {
	return &FairRWLock{
		queue:     newFairWaiterQueue(),
		createdAt: time.Now(),
	}
}

// RLock acquires the lock in reader mode.
func (rw *FairRWLock) RLock() {
	startTime := time.Now()
	var waiter *fairWaiter

	for {
		if waiter != nil {
			if waiter.signaled.Load() {
				if rw.tryAcquireSignaledReader() {
					rw.readerAcquires.Add(1)
					return
				}
				continue
			}

			if rw.tryAcquireQueuedReader(waiter) {
				waiter.cancelled.Store(true)
				rw.readerAcquires.Add(1)
				return
			}

			waiter.wait()
			rw.totalWaitTime.Add(time.Since(startTime).Nanoseconds())
			continue
		}

		state := rw.state.Load()
		if state&rwLockWriterBit == 0 && rw.queue.isEmpty() {
			if state >= rwLockMaxReaders {
				panic("fairrwlock: too many readers")
			}
			if rw.state.CompareAndSwap(state, state+1) {
				rw.currentReaders.Add(1)
				rw.readerAcquires.Add(1)
				return
			}
			continue
		}

		waiter = newFairWaiter(fairWaiterKindReader)
		rw.queue.enqueue(waiter)
		rw.readerWaits.Add(1)
	}
}

// RUnlock releases a read lock.
func (rw *FairRWLock) RUnlock() {
	for {
		state := rw.state.Load()
		readers := state & rwLockMaxReaders
		if readers == 0 {
			panic("fairrwlock: RUnlock of unlocked FairRWLock")
		}

		newState := state - 1
		if !rw.state.CompareAndSwap(state, newState) {
			continue
		}

		rw.currentReaders.Add(-1)
		if newState&rwLockMaxReaders == 0 {
			rw.wakeNextWaiters()
		}
		return
	}
}

// Lock acquires the lock in writer mode.
func (rw *FairRWLock) Lock() {
	startTime := time.Now()
	var waiter *fairWaiter

	for {
		if waiter != nil {
			if waiter.signaled.Load() {
				if rw.tryAcquireSignaledWriter() {
					rw.writerAcquires.Add(1)
					return
				}
				continue
			}

			if rw.tryAcquireQueuedWriter(waiter) {
				waiter.cancelled.Store(true)
				rw.writerAcquires.Add(1)
				return
			}

			waiter.wait()
			rw.totalWaitTime.Add(time.Since(startTime).Nanoseconds())
			continue
		}

		if rw.queue.isEmpty() && rw.state.CompareAndSwap(0, rwLockWriterBit) {
			rw.writerHeld.Store(1)
			rw.writerAcquires.Add(1)
			return
		}

		waiter = newFairWaiter(fairWaiterKindWriter)
		rw.queue.enqueue(waiter)
		rw.writerWaits.Add(1)
	}
}

// Unlock releases a write lock.
func (rw *FairRWLock) Unlock() {
	state := rw.state.Load()
	if state != rwLockWriterBit {
		panic("fairrwlock: Unlock of unlocked FairRWLock")
	}
	if !rw.state.CompareAndSwap(rwLockWriterBit, 0) {
		panic("fairrwlock: inconsistent state in Unlock")
	}

	rw.writerHeld.Store(0)
	rw.wakeNextWaiters()
}

// RLockContext acquires a read lock or returns ctx.Err() when cancelled.
func (rw *FairRWLock) RLockContext(ctx context.Context) error {
	startTime := time.Now()
	var waiter *fairWaiter

	for {
		if waiter != nil {
			if waiter.signaled.Load() {
				if rw.tryAcquireSignaledReader() {
					rw.readerAcquires.Add(1)
					return nil
				}
				continue
			}

			if rw.tryAcquireQueuedReader(waiter) {
				waiter.cancelled.Store(true)
				rw.readerAcquires.Add(1)
				return nil
			}

			if err := waiter.waitContext(ctx); err != nil {
				return err
			}
			rw.totalWaitTime.Add(time.Since(startTime).Nanoseconds())
			continue
		}

		state := rw.state.Load()
		if state&rwLockWriterBit == 0 && rw.queue.isEmpty() {
			if state < rwLockMaxReaders && rw.state.CompareAndSwap(state, state+1) {
				rw.currentReaders.Add(1)
				rw.readerAcquires.Add(1)
				return nil
			}
			continue
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		waiter = newFairWaiter(fairWaiterKindReader)
		rw.queue.enqueue(waiter)
		rw.readerWaits.Add(1)
	}
}

// LockContext acquires a write lock or returns ctx.Err() when cancelled.
func (rw *FairRWLock) LockContext(ctx context.Context) error {
	startTime := time.Now()
	var waiter *fairWaiter

	for {
		if waiter != nil {
			if waiter.signaled.Load() {
				if rw.tryAcquireSignaledWriter() {
					rw.writerAcquires.Add(1)
					return nil
				}
				continue
			}

			if rw.tryAcquireQueuedWriter(waiter) {
				waiter.cancelled.Store(true)
				rw.writerAcquires.Add(1)
				return nil
			}

			if err := waiter.waitContext(ctx); err != nil {
				return err
			}
			rw.totalWaitTime.Add(time.Since(startTime).Nanoseconds())
			continue
		}

		if rw.queue.isEmpty() && rw.state.CompareAndSwap(0, rwLockWriterBit) {
			rw.writerHeld.Store(1)
			rw.writerAcquires.Add(1)
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		waiter = newFairWaiter(fairWaiterKindWriter)
		rw.queue.enqueue(waiter)
		rw.writerWaits.Add(1)
	}
}

// TryRLock tries to acquire a read lock without waiting.
// It succeeds only when no writer is active and no waiter is queued.
func (rw *FairRWLock) TryRLock() bool {
	for {
		state := rw.state.Load()
		if state&rwLockWriterBit != 0 || !rw.queue.isEmpty() {
			return false
		}
		if state >= rwLockMaxReaders {
			return false
		}
		if rw.state.CompareAndSwap(state, state+1) {
			rw.currentReaders.Add(1)
			rw.readerAcquires.Add(1)
			return true
		}
	}
}

// TryLock tries to acquire a write lock without waiting.
// It succeeds only when no active holders exist and no waiter is queued.
func (rw *FairRWLock) TryLock() bool {
	if !rw.queue.isEmpty() {
		return false
	}
	if rw.state.CompareAndSwap(0, rwLockWriterBit) {
		rw.writerHeld.Store(1)
		rw.writerAcquires.Add(1)
		return true
	}
	return false
}

func (rw *FairRWLock) tryAcquireQueuedReader(waiter *fairWaiter) bool {
	head := rw.queue.peek()
	if head != waiter {
		return false
	}
	for {
		state := rw.state.Load()
		if state&rwLockWriterBit != 0 {
			return false
		}
		if state >= rwLockMaxReaders {
			panic("fairrwlock: too many readers")
		}
		if rw.state.CompareAndSwap(state, state+1) {
			rw.currentReaders.Add(1)
			return true
		}
	}
}

func (rw *FairRWLock) tryAcquireQueuedWriter(waiter *fairWaiter) bool {
	head := rw.queue.peek()
	if head != waiter {
		return false
	}
	if rw.state.CompareAndSwap(0, rwLockWriterBit) {
		rw.writerHeld.Store(1)
		return true
	}
	return false
}

func (rw *FairRWLock) tryAcquireSignaledReader() bool {
	for {
		state := rw.state.Load()
		if state&rwLockWriterBit != 0 {
			return false
		}
		if state >= rwLockMaxReaders {
			panic("fairrwlock: too many readers")
		}
		if rw.state.CompareAndSwap(state, state+1) {
			rw.currentReaders.Add(1)
			return true
		}
	}
}

func (rw *FairRWLock) tryAcquireSignaledWriter() bool {
	if rw.state.CompareAndSwap(0, rwLockWriterBit) {
		rw.writerHeld.Store(1)
		return true
	}
	return false
}

func (rw *FairRWLock) wakeNextWaiters() {
	head := rw.queue.peek()
	if head == nil {
		return
	}
	if head.kind == fairWaiterKindReader {
		for _, waiter := range rw.queue.dequeueReaders() {
			waiter.signal()
		}
		return
	}
	waiter := rw.queue.dequeue()
	if waiter != nil {
		waiter.signal()
	}
}

// GetStats returns runtime statistics about the FairRWLock.
func (rw *FairRWLock) GetStats() FairRWLockStats {
	return FairRWLockStats{
		Policy:          "fair-fifo",
		ReaderAcquires:  rw.readerAcquires.Load(),
		WriterAcquires:  rw.writerAcquires.Load(),
		ReaderWaits:     rw.readerWaits.Load(),
		WriterWaits:     rw.writerWaits.Load(),
		TotalWaitTimeNs: rw.totalWaitTime.Load(),
		CurrentReaders:  rw.currentReaders.Load(),
		WriterHeld:      rw.writerHeld.Load() == 1,
		WaitersQueued:   rw.queue.sizeValue(),
		Age:             time.Since(rw.createdAt),
	}
}

// String returns a string representation of FairRWLock statistics.
func (s FairRWLockStats) String() string {
	totalWaits := s.ReaderWaits + s.WriterWaits
	avgWaitNs := int64(0)
	if totalWaits > 0 {
		avgWaitNs = s.TotalWaitTimeNs / totalWaits
	}
	return fmt.Sprintf(
		"FairRWLock Stats:\n"+
			"  Policy: %s\n"+
			"  Reader Acquires: %d\n"+
			"  Writer Acquires: %d\n"+
			"  Reader Waits: %d\n"+
			"  Writer Waits: %d\n"+
			"  Avg Wait Time: %v\n"+
			"  Current Readers: %d\n"+
			"  Writer Held: %v\n"+
			"  Waiters Queued: %d\n"+
			"  Age: %v",
		s.Policy,
		s.ReaderAcquires,
		s.WriterAcquires,
		s.ReaderWaits,
		s.WriterWaits,
		time.Duration(avgWaitNs),
		s.CurrentReaders,
		s.WriterHeld,
		s.WaitersQueued,
		s.Age,
	)
}
