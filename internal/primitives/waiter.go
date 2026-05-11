package primitives

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Waiter represents a goroutine waiting on a synchronization primitive
type Waiter struct {
	ID        uint64
	Ready     chan struct{}
	CreatedAt time.Time
	cancelled atomic.Bool // set to true when the waiter has self-acquired the lock
}

type waiterNode struct {
	waiter *Waiter
	next   atomic.Pointer[waiterNode]
}

// WaiterQueue manages a queue of waiting goroutines using the Michael-Scott
// lock-free queue algorithm with a sentinel (dummy) head node.
// The sentinel eliminates the race between head CAS and tail.Store that
// existed in the original empty-queue Enqueue path.
type WaiterQueue struct {
	head atomic.Pointer[waiterNode]
	tail atomic.Pointer[waiterNode]
	size atomic.Int64
}

// waiterIDCounter is a global monotonic counter for assigning unique IDs to
// Waiter instances. It is intentionally global so that IDs are unique across
// all primitives and their wait queues in a process. The global is safe to
// use from concurrent goroutines because atomic.Uint64 operations are
// themselves atomic.
var waiterIDCounter atomic.Uint64

var waiterPool = sync.Pool{
	New: func() interface{} {
		return &Waiter{
			Ready: make(chan struct{}, 1),
		}
	},
}

func getWaiter() *Waiter {
	w := waiterPool.Get().(*Waiter)
	// Drain any stale signal from a previous lifecycle.
	select {
	case <-w.Ready:
	default:
	}
	w.ID = waiterIDCounter.Add(1)
	w.CreatedAt = time.Now()
	w.cancelled.Store(false)
	return w
}

func putWaiter(w *Waiter) {
	if w == nil {
		return
	}
	// Best-effort drain before returning to pool.
	select {
	case <-w.Ready:
	default:
	}
	w.cancelled.Store(false)
	waiterPool.Put(w)
}

// NewWaiter creates a new waiter
func NewWaiter() *Waiter {
	return getWaiter()
}

// NewWaiterQueue creates a new waiter queue with a sentinel node.
// The sentinel is never returned by Dequeue; it just anchors the list.
func NewWaiterQueue() *WaiterQueue {
	sentinel := &waiterNode{} // dummy head node
	q := &WaiterQueue{}
	q.head.Store(sentinel)
	q.tail.Store(sentinel)
	return q
}

// Enqueue adds a waiter to the queue using the Michael-Scott algorithm.
func (q *WaiterQueue) Enqueue(w *Waiter) {
	node := &waiterNode{waiter: w}
	for {
		tail := q.tail.Load()
		next := tail.next.Load()
		// Confirm tail hasn't changed
		if tail != q.tail.Load() {
			continue
		}
		if next == nil {
			// Tail is the true last node; try to link node after it.
			if tail.next.CompareAndSwap(nil, node) {
				// Link succeeded; try to advance tail (if it fails, next Enqueue fixes it)
				q.tail.CompareAndSwap(tail, node)
				q.size.Add(1)
				return
			}
		} else {
			// Tail is lagging; help advance it
			q.tail.CompareAndSwap(tail, next)
		}
	}
}

// Dequeue removes and returns the first real waiter from the queue.
// Skips cancelled waiters (those that self-acquired the lock after enqueuing).
// Returns nil if no non-cancelled waiter is present.
func (q *WaiterQueue) Dequeue() *Waiter {
	for {
		head := q.head.Load()
		tail := q.tail.Load()
		next := head.next.Load()
		// Confirm head hasn't changed
		if head != q.head.Load() {
			continue
		}
		if head == tail {
			// Queue is empty or tail is lagging
			if next == nil {
				return nil // truly empty
			}
			// Tail is behind; advance it
			q.tail.CompareAndSwap(tail, next)
			continue
		}
		// next is the first real node; read it before CAS
		item := next
		if q.head.CompareAndSwap(head, next) {
			q.size.Add(-1)
			waiter := item.waiter
			// Skip cancelled waiters: if this waiter cancelled itself, discard
			// and loop to get the next one.
			if waiter.cancelled.Load() {
				putWaiter(waiter)
				continue
			}
			return waiter
		}
	}
}

// Size returns the number of waiters logically in the queue
// (includes any not-yet-dequeued cancelled waiters, so may be slightly high).
func (q *WaiterQueue) Size() int64 {
	v := q.size.Load()
	if v < 0 {
		return 0
	}
	return v
}

// IsEmpty returns true if the queue has no pending waiters
func (q *WaiterQueue) IsEmpty() bool {
	return q.Size() == 0
}

// Signal wakes up a waiter
func (w *Waiter) Signal() {
	select {
	case w.Ready <- struct{}{}:
	default:
	}
}

// Wait blocks until the waiter is signaled
func (w *Waiter) Wait() {
	<-w.Ready
}

// WaitTimeout blocks until the waiter is signaled or timeout.
// Uses time.NewTimer (not time.After) so the timer is cancelled immediately
// on wakeup, preventing the goroutine leak that time.After causes.
func (w *Waiter) WaitTimeout(timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-w.Ready:
		return true
	case <-timer.C:
		return false
	}
}

// WaitContext blocks until the waiter is signaled or ctx is cancelled.
// Returns ctx.Err() if the context is cancelled before the waiter is signalled.
// On cancellation, sets waiter.cancelled so that a racing Signal/wakeOne skips it.
func (w *Waiter) WaitContext(ctx context.Context) error {
	select {
	case <-w.Ready:
		return nil
	case <-ctx.Done():
		w.cancelled.Store(true)
		return ctx.Err()
	}
}
