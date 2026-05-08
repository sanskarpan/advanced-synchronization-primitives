package primitives

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestMutexLostWakeup exercises the lost-wakeup window in Mutex.Lock.
//
// Pattern: goroutine A holds the mutex. Goroutine B fails the fast-path CAS
// and is mid-enqueue when A releases.  Without the post-enqueue re-check, B
// sleeps forever.  With the fix it must eventually acquire.
func TestMutexLostWakeup(t *testing.T) {
	const iterations = 5000
	for i := 0; i < iterations; i++ {
		m := NewMutex()
		m.Lock() // A holds

		done := make(chan struct{})
		go func() {
			defer close(done)
			m.Lock() // B must acquire after A unlocks
			m.Unlock() //lint:ignore SA2001 intentional: testing that B can acquire; no shared state to protect
		}()

		// Yield so B gets as far as it can into the slow path before we unlock
		runtime.Gosched()
		m.Unlock() // A releases — may race with B's Enqueue

		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("iteration %d: goroutine B is stuck (lost wakeup)", i)
		}
	}
}

// TestMutexConcurrentCorrectness verifies mutual exclusion under heavy
// concurrent load (also exercises the lost-wakeup path at high frequency).
func TestMutexConcurrentCorrectness(t *testing.T) {
	const goroutines = 20
	const iters = 500
	m := NewMutex()
	counter := 0
	var wg sync.WaitGroup

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				m.Lock()
				counter++
				m.Unlock()
			}
		}()
	}
	wg.Wait()

	if want := goroutines * iters; counter != want {
		t.Errorf("counter=%d want %d (mutual exclusion violated)", counter, want)
	}
}

// TestSemaphoreLostWakeup exercises the lost-wakeup window in AcquireN.
//
// Pattern: semaphore is at 0, goroutine B checks count (0 < n) then a
// Release fires before B enqueues.  Without re-check B sleeps forever.
func TestSemaphoreLostWakeup(t *testing.T) {
	const iterations = 3000
	for i := 0; i < iterations; i++ {
		sem := NewSemaphore(1)
		sem.Acquire() // drain to 0

		done := make(chan struct{})
		go func() {
			defer close(done)
			sem.Acquire() // must unblock after Release
		}()

		runtime.Gosched()
		sem.Release() // may race with Acquire's Enqueue

		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("iteration %d: semaphore Acquire stuck (lost wakeup)", i)
		}
	}
}

// TestRWLockLostWakeup exercises lost-wakeup windows in both RLock and Lock.
// Many goroutines hammer RLock/Lock/Unlock rapidly.  Any permanent sleep
// would stall the WaitGroup forever and the test would timeout.
func TestRWLockLostWakeup(t *testing.T) {
	const goroutines = 16
	const iters = 300

	rw := NewRWLock()
	var wg sync.WaitGroup
	var readerCount atomic.Int64
	var writerCount atomic.Int64

	// mix of readers and writers
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		isWriter := g%4 == 0 // 25% writers
		go func(writer bool) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				if writer {
					rw.Lock()
					writerCount.Add(1)
					runtime.Gosched()
					rw.Unlock()
				} else {
					rw.RLock()
					readerCount.Add(1)
					runtime.Gosched()
					rw.RUnlock()
				}
			}
		}(isWriter)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("RWLock goroutines stuck — possible lost wakeup (readers=%d writers=%d)",
			readerCount.Load(), writerCount.Load())
	}
}

// TestWaiterQueueConcurrent verifies that the Michael-Scott queue is correct
// under concurrent enqueue/dequeue on an initially-empty queue.
//
// Checks:
//   - queue size never goes negative
//   - no panics (which would indicate corruption)
//   - total items dequeued == total items enqueued (all waiters accounted for)
func TestWaiterQueueConcurrent(t *testing.T) {
	const producers = 8
	const consumers = 8
	const itemsPerProducer = 500

	q := NewWaiterQueue()
	var enqueued atomic.Int64
	var dequeued atomic.Int64
	var negativeSize atomic.Bool

	var wg sync.WaitGroup

	// Producers: enqueue items
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < itemsPerProducer; i++ {
				w := NewWaiter()
				q.Enqueue(w)
				enqueued.Add(1)

				sz := q.Size()
				if sz < 0 {
					negativeSize.Store(true)
				}
			}
		}()
	}

	// Consumers: dequeue items; non-nil returns count as consumed
	total := int64(producers * itemsPerProducer)
	for c := 0; c < consumers; c++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for dequeued.Load() < total {
				w := q.Dequeue()
				if w != nil {
					dequeued.Add(1)
					// Signal the waiter so it does not leak
					w.Signal()
				} else {
					runtime.Gosched()
				}
			}
		}()
	}

	wg.Wait()

	if negativeSize.Load() {
		t.Error("WaiterQueue size went negative — queue corruption detected")
	}

	// Drain any remaining items
	for {
		w := q.Dequeue()
		if w == nil {
			break
		}
		dequeued.Add(1)
		w.Signal()
	}

	if enq, deq := enqueued.Load(), dequeued.Load(); enq != deq {
		t.Errorf("enqueued=%d dequeued=%d: some waiters were lost or duplicated", enq, deq)
	}
}

// TestBarrierWaitLostWakeup exercises the lost-wakeup window in Barrier.Wait.
//
// Pattern: N-1 goroutines are between arrived.Add(1) and waiters.Enqueue when
// the Nth goroutine trips the barrier and calls wakeAll on an empty queue.
// Without the post-enqueue generation re-check those goroutines sleep forever.
func TestBarrierWaitLostWakeup(t *testing.T) {
	const iterations = 1000
	const parties = int32(3)

	for i := 0; i < iterations; i++ {
		b := NewBarrier(parties)
		var wg sync.WaitGroup

		for p := int32(0); p < parties; p++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				b.Wait()
			}()
		}

		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("iteration %d: goroutine stuck in Barrier.Wait (lost wakeup)", i)
		}
	}
}

// TestBarrierWaitTimeoutDecrement verifies that WaitTimeout never leaves
// the arrived counter below 0, even when timeouts race with barrier trips.
func TestBarrierWaitTimeoutDecrement(t *testing.T) {
	const iterations = 200
	const parties = int32(4)
	const workers = 8 // more workers than parties

	for iter := 0; iter < iterations; iter++ {
		b := NewBarrier(parties)
		var wg sync.WaitGroup

		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				// Very short timeout races against other goroutines completing
				b.WaitTimeout(time.Millisecond)
			}()
		}

		wg.Wait()

		arrived := b.GetArrived()
		if arrived < 0 {
			t.Fatalf("iteration %d: arrived=%d < 0 (counter corrupted)", iter, arrived)
		}
	}
}

// TestBarrierWaitTimeoutDecrementRace verifies that concurrent WaitTimeout
// calls with varying timeouts never leave arrived < 0.
// All goroutines use WaitTimeout (no blocking Wait), so none can deadlock.
func TestBarrierWaitTimeoutDecrementRace(t *testing.T) {
	const iterations = 200
	const parties = int32(4)
	const workers = 20 // >> parties; many will timeout

	for iter := 0; iter < iterations; iter++ {
		b := NewBarrier(parties)
		var wg sync.WaitGroup

		for w := 0; w < workers; w++ {
			wg.Add(1)
			// Vary timeouts: some expire quickly, some give the barrier
			// time to trip naturally (exactly parties goroutines may hit
			// it before others timeout).
			timeout := time.Duration(w%3+1) * time.Millisecond
			go func(to time.Duration) {
				defer wg.Done()
				b.WaitTimeout(to)
			}(timeout)
		}

		wg.Wait()

		arrived := b.GetArrived()
		if arrived < 0 {
			t.Fatalf("iteration %d: arrived=%d < 0 (counter corrupted)", iter, arrived)
		}
	}
}
