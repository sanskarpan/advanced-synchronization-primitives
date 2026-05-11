package primitives

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"
)

// Semaphore is a counting semaphore implemented from atomic operations
// It maintains a count and allows up to N concurrent acquisitions
type Semaphore struct {
	capacity int32
	count    atomic.Int32 // current available count
	waiters  *WaiterQueue

	// Metrics
	acquires      atomic.Int64
	releases      atomic.Int64
	waits         atomic.Int64
	timeouts      atomic.Int64
	totalWaitTime atomic.Int64 // nanoseconds
	createdAt     time.Time
}

// NewSemaphore creates a new semaphore with the given capacity
func NewSemaphore(capacity int32) *Semaphore {
	if capacity <= 0 {
		panic("semaphore: capacity must be positive")
	}

	s := &Semaphore{
		capacity:  capacity,
		waiters:   NewWaiterQueue(),
		createdAt: time.Now(),
	}
	s.count.Store(capacity)

	return s
}

// Acquire acquires one resource from the semaphore, blocking if necessary
func (s *Semaphore) Acquire() {
	s.AcquireN(1)
}

// AcquireN acquires n resources from the semaphore, blocking if necessary.
//
// Lost-wakeup fix: after enqueuing a waiter we re-check the count under a
// CAS.  If count became >= n between our failed count-check and the Enqueue
// call, we acquire directly and mark the phantom waiter as cancelled.
func (s *Semaphore) AcquireN(n int32) {
	if n <= 0 {
		panic("semaphore: n must be positive")
	}
	if n > s.capacity {
		panic("semaphore: n exceeds capacity")
	}

	startTime := time.Now()

	for {
		count := s.count.Load()

		if count >= n {
			// Try to acquire
			if s.count.CompareAndSwap(count, count-n) {
				s.acquires.Add(int64(n))
				return
			}
			// CAS lost, retry without enqueuing
			continue
		}

		// Need to wait: enqueue first, then re-check to close the
		// lost-wakeup window between the count check above and Enqueue.
		waiter := NewWaiter()
		s.waiters.Enqueue(waiter)
		s.waits.Add(1)

		// Re-check after enqueue: a Release may have run in the window.
		if cur := s.count.Load(); cur >= n {
			if s.count.CompareAndSwap(cur, cur-n) {
				// Acquired: cancel the phantom waiter so wakeOne skips it.
				waiter.cancelled.Store(true)
				s.acquires.Add(int64(n))
				return
			}
		}

		waiter.Wait()
		putWaiter(waiter)

		waitTime := time.Since(startTime).Nanoseconds()
		s.totalWaitTime.Add(waitTime)

		// After being woken, loop to retry the CAS
	}
}

// Release releases one resource back to the semaphore.
// Returns an error if the release would exceed capacity (instead of panicking),
// allowing callers to handle over-release gracefully.
func (s *Semaphore) Release() error {
	return s.ReleaseN(1)
}

// ReleaseN releases n resources back to the semaphore.
// Returns an error if the release would exceed capacity; panics only for
// programmer errors (n <= 0).
func (s *Semaphore) ReleaseN(n int32) error {
	if n <= 0 {
		panic("semaphore: n must be positive")
	}

	for {
		count := s.count.Load()

		if count+n > s.capacity {
			return fmt.Errorf("semaphore: release would exceed capacity (count=%d, n=%d, capacity=%d)", count, n, s.capacity)
		}

		// Try to release
		if s.count.CompareAndSwap(count, count+n) {
			s.releases.Add(int64(n))

			// Wake up waiting goroutines
			for i := int32(0); i < n; i++ {
				s.wakeOne()
			}

			return nil
		}
	}
}

// TryAcquire tries to acquire one resource without blocking
func (s *Semaphore) TryAcquire() bool {
	return s.TryAcquireN(1)
}

// TryAcquireN tries to acquire n resources without blocking
func (s *Semaphore) TryAcquireN(n int32) bool {
	if n <= 0 {
		panic("semaphore: n must be positive")
	}
	if n > s.capacity {
		return false
	}

	for {
		count := s.count.Load()

		if count < n {
			return false
		}

		// Try to acquire
		if s.count.CompareAndSwap(count, count-n) {
			s.acquires.Add(int64(n))
			return true
		}
	}
}

// AcquireTimeout tries to acquire one resource with a timeout
func (s *Semaphore) AcquireTimeout(timeout time.Duration) bool {
	return s.AcquireNTimeout(1, timeout)
}

// AcquireNTimeout tries to acquire n resources with a timeout.
//
// Same lost-wakeup fix as AcquireN: re-check after enqueue.
func (s *Semaphore) AcquireNTimeout(n int32, timeout time.Duration) bool {
	if n <= 0 {
		panic("semaphore: n must be positive")
	}
	if n > s.capacity {
		return false
	}

	startTime := time.Now()
	deadline := startTime.Add(timeout)

	for {
		count := s.count.Load()

		if count >= n {
			// Try to acquire
			if s.count.CompareAndSwap(count, count-n) {
				s.acquires.Add(int64(n))
				return true
			}
			continue
		}

		// Check timeout
		if time.Now().After(deadline) {
			s.timeouts.Add(1)
			return false
		}

		// Need to wait
		waiter := NewWaiter()
		s.waiters.Enqueue(waiter)
		s.waits.Add(1)

		// Re-check after enqueue (lost-wakeup window)
		if cur := s.count.Load(); cur >= n {
			if s.count.CompareAndSwap(cur, cur-n) {
				waiter.cancelled.Store(true)
				s.acquires.Add(int64(n))
				return true
			}
		}

		remainingTime := time.Until(deadline)
		if !waiter.WaitTimeout(remainingTime) {
			// Timed out: cancel the waiter so wakeOne skips it.
			waiter.cancelled.Store(true)
			s.timeouts.Add(1)
			return false
		}
		putWaiter(waiter)

		waitTime := time.Since(startTime).Nanoseconds()
		s.totalWaitTime.Add(waitTime)

		// After being woken up, try again
	}
}

// AcquireContext acquires one resource or returns ctx.Err() on cancellation.
func (s *Semaphore) AcquireContext(ctx context.Context) error {
	return s.AcquireNContext(ctx, 1)
}

// AcquireNContext acquires n resources or returns ctx.Err() on cancellation.
func (s *Semaphore) AcquireNContext(ctx context.Context, n int32) error {
	if n <= 0 {
		panic("semaphore: n must be positive")
	}
	if n > s.capacity {
		panic("semaphore: n exceeds capacity")
	}

	startTime := time.Now()

	for {
		count := s.count.Load()
		if count >= n {
			if s.count.CompareAndSwap(count, count-n) {
				s.acquires.Add(int64(n))
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
		s.waiters.Enqueue(waiter)
		s.waits.Add(1)

		// Re-check after enqueue.
		if cur := s.count.Load(); cur >= n {
			if s.count.CompareAndSwap(cur, cur-n) {
				waiter.cancelled.Store(true)
				s.acquires.Add(int64(n))
				return nil
			}
		}

		if err := waiter.WaitContext(ctx); err != nil {
			// WaitContext already set cancelled.
			return err
		}
		putWaiter(waiter)

		waitTime := time.Since(startTime).Nanoseconds()
		s.totalWaitTime.Add(waitTime)
	}
}

// wakeOne wakes up one waiting goroutine.
// WaiterQueue.Dequeue already skips cancelled entries.
func (s *Semaphore) wakeOne() {
	waiter := s.waiters.Dequeue()
	if waiter != nil {
		waiter.Signal()
	}
}

// GetCount returns the current available count
func (s *Semaphore) GetCount() int32 {
	return s.count.Load()
}

// GetCapacity returns the semaphore capacity
func (s *Semaphore) GetCapacity() int32 {
	return s.capacity
}

// GetStats returns statistics about the semaphore
func (s *Semaphore) GetStats() SemaphoreStats {
	return SemaphoreStats{
		Capacity:        s.capacity,
		CurrentCount:    s.count.Load(),
		Acquires:        s.acquires.Load(),
		Releases:        s.releases.Load(),
		Waits:           s.waits.Load(),
		Timeouts:        s.timeouts.Load(),
		TotalWaitTimeNs: s.totalWaitTime.Load(),
		WaitersQueued:   s.waiters.Size(),
		Age:             time.Since(s.createdAt),
	}
}

// SemaphoreStats contains statistics about a semaphore
type SemaphoreStats struct {
	Capacity        int32
	CurrentCount    int32
	Acquires        int64
	Releases        int64
	Waits           int64
	Timeouts        int64
	TotalWaitTimeNs int64
	WaitersQueued   int64
	Age             time.Duration
}

// String returns a string representation of the stats
func (s SemaphoreStats) String() string {
	avgWait := time.Duration(0)
	if s.Waits > 0 {
		avgWait = time.Duration(s.TotalWaitTimeNs / s.Waits)
	}

	return fmt.Sprintf(
		"Semaphore Stats:\n"+
			"  Capacity: %d\n"+
			"  Current Count: %d\n"+
			"  Utilization: %.1f%%\n"+
			"  Acquires: %d\n"+
			"  Releases: %d\n"+
			"  Waits: %d\n"+
			"  Timeouts: %d\n"+
			"  Avg Wait Time: %v\n"+
			"  Waiters Queued: %d\n"+
			"  Age: %v",
		s.Capacity,
		s.CurrentCount,
		100.0*float64(s.Capacity-s.CurrentCount)/float64(s.Capacity),
		s.Acquires,
		s.Releases,
		s.Waits,
		s.Timeouts,
		avgWait,
		s.WaitersQueued,
		s.Age,
	)
}
