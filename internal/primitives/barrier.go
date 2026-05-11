package primitives

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"
)

// ErrBarrierBroken is returned by Wait, WaitTimeout, and WaitContext when the
// barrier has been broken via Break().
var ErrBarrierBroken = errors.New("barrier broken")

// Barrier is a synchronization barrier that blocks goroutines until
// a specified number of them reach the barrier point
type Barrier struct {
	parties    int32 // number of goroutines that must arrive
	arrived    atomic.Int32
	generation atomic.Int64 // generation number for barrier reuse
	waiters    *WaiterQueue

	// broken is set to true by Break(); waiting goroutines return ErrBarrierBroken.
	broken atomic.Bool

	// Metrics
	trips         atomic.Int64 // number of times barrier was tripped
	totalWaitTime atomic.Int64 // nanoseconds
	createdAt     time.Time
}

// NewBarrier creates a new barrier for the given number of parties
func NewBarrier(parties int32) *Barrier {
	if parties <= 0 {
		panic("barrier: parties must be positive")
	}

	return &Barrier{
		parties:   parties,
		waiters:   NewWaiterQueue(),
		createdAt: time.Now(),
	}
}

// Wait blocks until all parties have called Wait.
// Returns the arrival index (0 to parties-1) and nil on success.
// Returns -1 and ErrBarrierBroken if Break() was called while waiting.
//
// Lost-wakeup fix: after enqueuing a waiter we re-check the generation. If
// the barrier tripped while we were between arrived.Add and Enqueue, wakeAll
// already ran on an empty queue and will never signal us. Detecting the
// advanced generation here lets us cancel the phantom waiter and return.
func (b *Barrier) Wait() (int32, error) {
	startTime := time.Now()

	// Check broken before even trying.
	if b.broken.Load() {
		return -1, ErrBarrierBroken
	}

	// Capture generation BEFORE incrementing arrived so any trip that occurs
	// after our Add but before our Enqueue is visible in the re-check below.
	generation := b.generation.Load()

	// Increment arrived count
	arrived := b.arrived.Add(1)

	// Over-subscription guard: more goroutines called Wait than parties.
	// Undo the increment and return immediately with an invalid index.
	if arrived > b.parties {
		for {
			cur := b.arrived.Load()
			if cur <= 0 {
				break
			}
			if b.arrived.CompareAndSwap(cur, cur-1) {
				break
			}
		}
		return arrived - 1, nil
	}

	// Check if this is the last party
	if arrived == b.parties {
		// Trip the barrier
		b.trips.Add(1)

		// Reset for next use
		b.arrived.Store(0)
		b.generation.Add(1)

		// Wake all waiting goroutines
		b.wakeAll()

		return arrived - 1, nil
	}

	// Enqueue our waiter, then immediately re-check whether the barrier
	// already tripped while we were enqueuing (lost-wakeup window).
	waiter := NewWaiter()
	b.waiters.Enqueue(waiter)

	if b.generation.Load() > generation {
		// Barrier tripped before or during our Enqueue; wakeAll already ran.
		// Cancel the phantom waiter so a future wakeAll skips it.
		waiter.cancelled.Store(true)
		waitTime := time.Since(startTime).Nanoseconds()
		b.totalWaitTime.Add(waitTime)
		return arrived - 1, nil
	}

	waiter.Wait()
	putWaiter(waiter)

	// Check broken state after wakeup.
	if b.broken.Load() {
		return -1, ErrBarrierBroken
	}

	// Check if we're in a new generation (barrier was tripped)
	if b.generation.Load() > generation {
		waitTime := time.Since(startTime).Nanoseconds()
		b.totalWaitTime.Add(waitTime)
	}

	return arrived - 1, nil
}

// WaitTimeout is like Wait but with a timeout.
// Returns arrival index and nil if barrier was tripped.
// Returns -1 and false-equivalent if timeout, over-subscription, or broken.
// Returns -1 and ErrBarrierBroken if Break() was called while waiting.
//
// Lost-wakeup fix: same post-enqueue generation re-check as Wait.
//
// Decrement race fix: on timeout we only decrement arrived if we are still
// in the same generation.  If the generation has already advanced (the
// barrier tripped while we were timing out) then arrived has already been
// reset to 0 by the tripping goroutine and we must not touch it.
// A CAS loop ensures we never decrement a counter that was already reset.
func (b *Barrier) WaitTimeout(timeout time.Duration) (int32, bool) {
	startTime := time.Now()

	// Check broken before even trying.
	if b.broken.Load() {
		return -1, false
	}

	// Capture generation BEFORE incrementing so the post-enqueue re-check
	// can detect a trip that occurred in the Add→Enqueue window.
	generation := b.generation.Load()

	// Increment arrived count
	arrived := b.arrived.Add(1)

	// Over-subscription guard: more goroutines called WaitTimeout than parties.
	if arrived > b.parties {
		for {
			cur := b.arrived.Load()
			if cur <= 0 {
				break
			}
			if b.arrived.CompareAndSwap(cur, cur-1) {
				break
			}
		}
		return -1, false
	}

	// Check if this is the last party
	if arrived == b.parties {
		// Trip the barrier
		b.trips.Add(1)

		// Reset for next use
		b.arrived.Store(0)
		b.generation.Add(1)

		// Wake all waiting goroutines
		b.wakeAll()

		return arrived - 1, true
	}

	// Enqueue waiter then immediately re-check for a trip that occurred while
	// we were enqueuing (lost-wakeup window).
	waiter := NewWaiter()
	b.waiters.Enqueue(waiter)

	if b.generation.Load() > generation {
		// Barrier already tripped; cancel phantom waiter and return success.
		waiter.cancelled.Store(true)
		waitTime := time.Since(startTime).Nanoseconds()
		b.totalWaitTime.Add(waitTime)
		return arrived - 1, true
	}

	if !waiter.WaitTimeout(timeout) {
		// Timed out.
		// Only undo our arrived increment if the generation hasn't advanced.
		// If generation advanced the barrier already reset arrived=0; we must
		// not decrement (that would corrupt the next generation's count).
		// CAS loop so we never go below zero.
		if b.generation.Load() == generation {
			for {
				cur := b.arrived.Load()
				if cur <= 0 {
					break
				}
				if b.arrived.CompareAndSwap(cur, cur-1) {
					break
				}
			}
		}
		// Cancel phantom waiter so wakeAll skips it.
		waiter.cancelled.Store(true)
		return -1, false
	}
	putWaiter(waiter)

	// Check broken state after wakeup.
	if b.broken.Load() {
		return -1, false
	}

	// Check if we're in a new generation (barrier was tripped)
	if b.generation.Load() > generation {
		waitTime := time.Since(startTime).Nanoseconds()
		b.totalWaitTime.Add(waitTime)
		return arrived - 1, true
	}

	return -1, false
}

// WaitContext is like Wait but returns ctx.Err() if the context is cancelled
// before the barrier trips.
func (b *Barrier) WaitContext(ctx context.Context) (int32, error) {
	startTime := time.Now()

	// Check broken before even trying.
	if b.broken.Load() {
		return -1, ErrBarrierBroken
	}

	generation := b.generation.Load()
	arrived := b.arrived.Add(1)

	// Over-subscription guard.
	if arrived > b.parties {
		for {
			cur := b.arrived.Load()
			if cur <= 0 {
				break
			}
			if b.arrived.CompareAndSwap(cur, cur-1) {
				break
			}
		}
		return -1, context.DeadlineExceeded
	}

	if arrived == b.parties {
		b.trips.Add(1)
		b.arrived.Store(0)
		b.generation.Add(1)
		b.wakeAll()
		return arrived - 1, nil
	}

	waiter := NewWaiter()
	b.waiters.Enqueue(waiter)

	if b.generation.Load() > generation {
		waiter.cancelled.Store(true)
		waitTime := time.Since(startTime).Nanoseconds()
		b.totalWaitTime.Add(waitTime)
		return arrived - 1, nil
	}

	if err := waiter.WaitContext(ctx); err != nil {
		// Context cancelled: undo arrived increment if still same generation.
		if b.generation.Load() == generation {
			for {
				cur := b.arrived.Load()
				if cur <= 0 {
					break
				}
				if b.arrived.CompareAndSwap(cur, cur-1) {
					break
				}
			}
		}
		return -1, err
	}
	putWaiter(waiter)

	// Check broken state after wakeup.
	if b.broken.Load() {
		return -1, ErrBarrierBroken
	}

	if b.generation.Load() > generation {
		waitTime := time.Since(startTime).Nanoseconds()
		b.totalWaitTime.Add(waitTime)
	}

	return arrived - 1, nil
}

// Break breaks the barrier, waking all current waiters with ErrBarrierBroken.
// After Break, all subsequent Wait/WaitTimeout/WaitContext calls return an
// error until Reset() is called.
func (b *Barrier) Break() {
	b.broken.Store(true)
	// Wake all waiting goroutines so they unblock and check broken.
	for {
		waiter := b.waiters.Dequeue()
		if waiter == nil {
			return
		}
		waiter.Signal()
	}
}

// IsBroken returns true if the barrier has been broken.
func (b *Barrier) IsBroken() bool {
	return b.broken.Load()
}

// Reset manually resets the barrier, also clearing any broken state.
// WARNING: Should only be called when no goroutines are waiting.
func (b *Barrier) Reset() {
	b.broken.Store(false)
	b.arrived.Store(0)
	b.generation.Add(1)

	// Clear waiting goroutines (structural drain, same rationale as wakeAll).
	for {
		waiter := b.waiters.Dequeue()
		if waiter == nil {
			return
		}
		waiter.Signal()
	}
}

// wakeAll wakes up all waiting goroutines.
//
// Uses structural drain (loop until Dequeue returns nil) rather than checking
// IsEmpty first.  IsEmpty uses the size counter which is incremented AFTER a
// node is linked into the queue; in the window between the link CAS and the
// size.Add a concurrent wakeAll would see size=0 and exit prematurely.
// Dequeue itself operates on the structural head/next pointers and returns nil
// only when the queue is truly empty at the structural level.
func (b *Barrier) wakeAll() {
	for {
		waiter := b.waiters.Dequeue()
		if waiter == nil {
			return
		}
		waiter.Signal()
	}
}

// GetParties returns the number of parties required to trip the barrier
func (b *Barrier) GetParties() int32 {
	return b.parties
}

// GetArrived returns the number of parties currently waiting
func (b *Barrier) GetArrived() int32 {
	return b.arrived.Load()
}

// GetGeneration returns the current generation number
func (b *Barrier) GetGeneration() int64 {
	return b.generation.Load()
}

// GetStats returns statistics about the barrier
func (b *Barrier) GetStats() BarrierStats {
	return BarrierStats{
		Parties:         b.parties,
		Arrived:         b.arrived.Load(),
		Generation:      b.generation.Load(),
		Trips:           b.trips.Load(),
		TotalWaitTimeNs: b.totalWaitTime.Load(),
		WaitersQueued:   b.waiters.Size(),
		Age:             time.Since(b.createdAt),
	}
}

// BarrierStats contains statistics about a barrier
type BarrierStats struct {
	Parties         int32
	Arrived         int32
	Generation      int64
	Trips           int64
	TotalWaitTimeNs int64
	WaitersQueued   int64
	Age             time.Duration
}

// String returns a string representation of the stats
func (s BarrierStats) String() string {
	avgWait := time.Duration(0)
	totalWaiters := s.Trips * int64(s.Parties-1) // Last party doesn't wait
	if totalWaiters > 0 {
		avgWait = time.Duration(s.TotalWaitTimeNs / totalWaiters)
	}

	progress := 0.0
	if s.Parties > 0 {
		progress = 100.0 * float64(s.Arrived) / float64(s.Parties)
	}

	return fmt.Sprintf(
		"Barrier Stats:\n"+
			"  Parties: %d\n"+
			"  Arrived: %d\n"+
			"  Progress: %.1f%%\n"+
			"  Generation: %d\n"+
			"  Trips: %d\n"+
			"  Avg Wait Time: %v\n"+
			"  Waiters Queued: %d\n"+
			"  Age: %v",
		s.Parties,
		s.Arrived,
		progress,
		s.Generation,
		s.Trips,
		avgWait,
		s.WaitersQueued,
		s.Age,
	)
}
