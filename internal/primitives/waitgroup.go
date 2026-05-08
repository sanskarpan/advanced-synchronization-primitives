package primitives

import (
	"context"
	"sync/atomic"
	"time"
)

// WaitGroup is an instrumented wait group with context-cancellable Wait.
type WaitGroup struct {
	counter  atomic.Int32
	waiters  *WaiterQueue
	mu       atomic.Int32 // internal spinlock for wakeAll coordination (0=free,1=held)
	createdAt time.Time

	// Metrics
	addCount  atomic.Int64
	doneCount atomic.Int64
	waitCount atomic.Int64
}

// WaitGroupStats contains statistics about a WaitGroup.
type WaitGroupStats struct {
	Count     int32
	AddCount  int64
	DoneCount int64
	WaitCount int64
	Age       time.Duration
}

// NewWaitGroup creates a new WaitGroup.
func NewWaitGroup() *WaitGroup {
	return &WaitGroup{
		waiters:   NewWaiterQueue(),
		createdAt: time.Now(),
	}
}

// Add adds delta to the WaitGroup counter.
// Panics if the counter goes negative.
func (wg *WaitGroup) Add(delta int) {
	n := wg.counter.Add(int32(delta))
	if n < 0 {
		panic("waitgroup: negative counter")
	}
	wg.addCount.Add(int64(delta))
	if n == 0 {
		wg.wakeAll()
	}
}

// Done decrements the WaitGroup counter by 1.
func (wg *WaitGroup) Done() {
	n := wg.counter.Add(-1)
	if n < 0 {
		panic("waitgroup: negative counter")
	}
	wg.doneCount.Add(1)
	if n == 0 {
		wg.wakeAll()
	}
}

// Wait blocks until the counter is zero.
func (wg *WaitGroup) Wait() {
	if wg.counter.Load() == 0 {
		return
	}

	wg.waitCount.Add(1)

	waiter := NewWaiter()
	wg.waiters.Enqueue(waiter)

	// Re-check after enqueue to close lost-wakeup window.
	if wg.counter.Load() == 0 {
		waiter.cancelled.Store(true)
		return
	}

	waiter.Wait()
}

// WaitContext blocks until the counter is zero or ctx is cancelled.
func (wg *WaitGroup) WaitContext(ctx context.Context) error {
	if wg.counter.Load() == 0 {
		return nil
	}

	wg.waitCount.Add(1)

	waiter := NewWaiter()
	wg.waiters.Enqueue(waiter)

	// Re-check after enqueue.
	if wg.counter.Load() == 0 {
		waiter.cancelled.Store(true)
		return nil
	}

	return waiter.WaitContext(ctx)
}

// GetCount returns the current counter value.
func (wg *WaitGroup) GetCount() int32 {
	return wg.counter.Load()
}

// GetStats returns statistics about the WaitGroup.
func (wg *WaitGroup) GetStats() WaitGroupStats {
	return WaitGroupStats{
		Count:     wg.counter.Load(),
		AddCount:  wg.addCount.Load(),
		DoneCount: wg.doneCount.Load(),
		WaitCount: wg.waitCount.Load(),
		Age:       time.Since(wg.createdAt),
	}
}

// wakeAll signals all current waiters.
func (wg *WaitGroup) wakeAll() {
	for {
		w := wg.waiters.Dequeue()
		if w == nil {
			return
		}
		w.Signal()
	}
}
