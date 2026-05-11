package primitives

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Compile-time check: *Mutex implements sync.Locker.
var _ sync.Locker = (*Mutex)(nil)

// Mutex is a simple mutual exclusion lock implemented from atomic operations
type Mutex struct {
	state   atomic.Int32 // 0 = unlocked, 1 = locked
	waiters *WaiterQueue

	// Metrics
	locks         atomic.Int64
	unlocks       atomic.Int64
	waits         atomic.Int64
	totalWaitTime atomic.Int64
	createdAt     time.Time
}

// NewMutex creates a new mutex
func NewMutex() *Mutex {
	return &Mutex{
		waiters:   NewWaiterQueue(),
		createdAt: time.Now(),
	}
}

// Lock acquires the mutex.
//
// Lost-wakeup fix: after enqueuing a waiter we re-check the state under a
// CAS.  If the mutex was released between our failed fast-path CAS and the
// Enqueue call we grab it directly and mark our waiter as cancelled so that
// Unlock's dequeue-and-signal loop skips it without handing off to nobody.
//
// Unlock uses a non-handoff model (always stores 0 before signaling) so that
// a cancelled waiter dequeued by a future Unlock does not leave state==1
// permanently.
func (m *Mutex) Lock() {
	startTime := time.Now()

	for {
		// Fast path: try to acquire immediately
		if m.state.CompareAndSwap(0, 1) {
			m.locks.Add(1)
			return
		}

		// Slow path: enqueue a waiter, then re-check to close the lost-wakeup
		// window between the failed CAS above and the Enqueue below.
		waiter := NewWaiter()
		m.waiters.Enqueue(waiter)
		m.waits.Add(1)

		// Re-check: if state is now 0 (Unlock fired while we were enqueuing)
		// grab the lock ourselves and cancel the phantom waiter entry.
		if m.state.CompareAndSwap(0, 1) {
			// Mark cancelled so Dequeue in a future Unlock skips it.
			waiter.cancelled.Store(true)
			m.locks.Add(1)
			return
		}

		// Block until Unlock signals us.
		waiter.Wait()

		waitTime := time.Since(startTime).Nanoseconds()
		m.totalWaitTime.Add(waitTime)
		// Non-handoff: state was already set to 0 by Unlock; loop to re-CAS.
	}
}

// Unlock releases the mutex.
//
// Non-handoff design: always sets state=0 first, then wakes one waiter.
// This means the woken goroutine must re-acquire via CAS (the loop in Lock).
// The non-handoff model is required so that a cancelled-waiter dequeue by
// Unlock does not leave state==1 with no owner.
func (m *Mutex) Unlock() {
	if m.state.Load() != 1 {
		panic("mutex: unlock of unlocked mutex")
	}

	m.unlocks.Add(1)

	// Release the lock first (non-handoff).
	m.state.Store(0)

	// Wake one non-cancelled waiter so it can re-CAS.
	// WaiterQueue.Dequeue already skips cancelled entries.
	waiter := m.waiters.Dequeue()
	if waiter != nil {
		waiter.Signal()
	}
}

// LockContext acquires the mutex or returns ctx.Err() if the context is
// cancelled before the lock can be acquired.
func (m *Mutex) LockContext(ctx context.Context) error {
	startTime := time.Now()

	for {
		// Fast path: try to acquire immediately.
		if m.state.CompareAndSwap(0, 1) {
			m.locks.Add(1)
			return nil
		}

		// Check context before blocking.
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Enqueue, then re-check.
		waiter := NewWaiter()
		m.waiters.Enqueue(waiter)
		m.waits.Add(1)

		if m.state.CompareAndSwap(0, 1) {
			waiter.cancelled.Store(true)
			m.locks.Add(1)
			return nil
		}

		if err := waiter.WaitContext(ctx); err != nil {
			// WaitContext already set waiter.cancelled.
			return err
		}

		waitTime := time.Since(startTime).Nanoseconds()
		m.totalWaitTime.Add(waitTime)
	}
}

// TryLock tries to acquire the mutex without blocking
func (m *Mutex) TryLock() bool {
	if m.state.CompareAndSwap(0, 1) {
		m.locks.Add(1)
		return true
	}
	return false
}

// IsLocked returns true if the mutex is currently locked
func (m *Mutex) IsLocked() bool {
	return m.state.Load() == 1
}

// CondVar is a condition variable implemented from atomic operations
// It allows goroutines to wait for certain conditions to be met
type CondVar struct {
	waiters *WaiterQueue

	// Metrics
	waits         atomic.Int64
	signals       atomic.Int64
	broadcasts    atomic.Int64
	totalWaitTime atomic.Int64
	createdAt     time.Time
}

// NewCondVar creates a new condition variable
func NewCondVar() *CondVar {
	return &CondVar{
		waiters:   NewWaiterQueue(),
		createdAt: time.Now(),
	}
}

// Wait atomically unlocks the mutex and waits for a signal.
// When woken up, it re-acquires the mutex before returning.
//
// IMPORTANT: callers MUST use Wait in a loop to guard against spurious wakeups:
//
//	m.Lock()
//	for !condition() {
//	    cv.Wait(m)
//	}
//	// condition is now true
//	m.Unlock()
//
// Use WaitFor when you have a simple boolean condition to check.
func (cv *CondVar) Wait(m *Mutex) {
	if !m.IsLocked() {
		panic("condvar: Wait called on unlocked mutex")
	}

	startTime := time.Now()

	waiter := NewWaiter()
	cv.waiters.Enqueue(waiter)
	cv.waits.Add(1)

	// Unlock the mutex
	m.Unlock()

	// Wait for signal
	waiter.Wait()

	waitTime := time.Since(startTime).Nanoseconds()
	cv.totalWaitTime.Add(waitTime)

	// Re-acquire the mutex
	m.Lock()
}

// WaitTimeout is like Wait but with a timeout
// Returns true if woken by signal, false if timeout
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
		// Timed out: cancel the waiter so a future Signal/Broadcast skip it.
		waiter.cancelled.Store(true)
	}
	waitTime := time.Since(startTime).Nanoseconds()
	cv.totalWaitTime.Add(waitTime)

	// Re-acquire the mutex
	m.Lock()

	return signaled
}

// WaitFor loops calling Wait(m) until cond() returns true.
// It provides spurious-wakeup safety for callers with a boolean predicate.
// The mutex m must be held when WaitFor is called; it is held on return.
func (cv *CondVar) WaitFor(m *Mutex, cond func() bool) {
	for !cond() {
		cv.Wait(m)
	}
}

// Signal wakes up one waiting goroutine
func (cv *CondVar) Signal() {
	waiter := cv.waiters.Dequeue()
	if waiter != nil {
		cv.signals.Add(1)
		waiter.Signal()
	}
}

// Broadcast wakes up all waiting goroutines.
// Uses structural drain (Dequeue until nil) instead of IsEmpty to avoid
// the size-counter lag where a mid-enqueue node is invisible to IsEmpty.
func (cv *CondVar) Broadcast() {
	count := int64(0)

	for {
		waiter := cv.waiters.Dequeue()
		if waiter == nil {
			break
		}
		waiter.Signal()
		count++
	}

	if count > 0 {
		cv.broadcasts.Add(1)
	}
}

// GetStats returns statistics about the condition variable
func (cv *CondVar) GetStats() CondVarStats {
	return CondVarStats{
		Waits:           cv.waits.Load(),
		Signals:         cv.signals.Load(),
		Broadcasts:      cv.broadcasts.Load(),
		TotalWaitTimeNs: cv.totalWaitTime.Load(),
		WaitersQueued:   cv.waiters.Size(),
		Age:             time.Since(cv.createdAt),
	}
}

// CondVarStats contains statistics about a condition variable
type CondVarStats struct {
	Waits           int64
	Signals         int64
	Broadcasts      int64
	TotalWaitTimeNs int64
	WaitersQueued   int64
	Age             time.Duration
}

// String returns a string representation of the stats
func (s CondVarStats) String() string {
	avgWait := time.Duration(0)
	if s.Waits > 0 {
		avgWait = time.Duration(s.TotalWaitTimeNs / s.Waits)
	}

	return fmt.Sprintf(
		"CondVar Stats:\n"+
			"  Waits: %d\n"+
			"  Signals: %d\n"+
			"  Broadcasts: %d\n"+
			"  Avg Wait Time: %v\n"+
			"  Waiters Queued: %d\n"+
			"  Age: %v",
		s.Waits,
		s.Signals,
		s.Broadcasts,
		avgWait,
		s.WaitersQueued,
		s.Age,
	)
}

// GetMutexStats returns statistics about the mutex
func (m *Mutex) GetStats() MutexStats {
	return MutexStats{
		Locks:           m.locks.Load(),
		Unlocks:         m.unlocks.Load(),
		Waits:           m.waits.Load(),
		TotalWaitTimeNs: m.totalWaitTime.Load(),
		IsLocked:        m.IsLocked(),
		WaitersQueued:   m.waiters.Size(),
		Age:             time.Since(m.createdAt),
	}
}

// MutexStats contains statistics about a mutex
type MutexStats struct {
	Locks           int64
	Unlocks         int64
	Waits           int64
	TotalWaitTimeNs int64
	IsLocked        bool
	WaitersQueued   int64
	Age             time.Duration
}

// String returns a string representation of the stats
func (s MutexStats) String() string {
	avgWait := time.Duration(0)
	if s.Waits > 0 {
		avgWait = time.Duration(s.TotalWaitTimeNs / s.Waits)
	}

	return fmt.Sprintf(
		"Mutex Stats:\n"+
			"  Locks: %d\n"+
			"  Unlocks: %d\n"+
			"  Waits: %d\n"+
			"  Avg Wait Time: %v\n"+
			"  Is Locked: %v\n"+
			"  Waiters Queued: %d\n"+
			"  Age: %v",
		s.Locks,
		s.Unlocks,
		s.Waits,
		avgWait,
		s.IsLocked,
		s.WaitersQueued,
		s.Age,
	)
}
