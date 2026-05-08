package primitives

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestCondVarSignal(t *testing.T) {
	m := NewMutex()
	cv := NewCondVar()

	var signaled sync.Mutex
	signaledVal := false
	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()
		m.Lock()
		cv.Wait(m)
		signaled.Lock()
		signaledVal = true
		signaled.Unlock()
		m.Unlock()
	}()

	time.Sleep(50 * time.Millisecond)

	signaled.Lock()
	if signaledVal {
		t.Error("Should not be signaled yet")
	}
	signaled.Unlock()

	cv.Signal()

	wg.Wait()

	signaled.Lock()
	if !signaledVal {
		t.Error("Should be signaled")
	}
	signaled.Unlock()
}

func TestCondVarBroadcast(t *testing.T) {
	m := NewMutex()
	cv := NewCondVar()

	numWaiters := 5
	signaled := 0
	var mu sync.Mutex

	var wg sync.WaitGroup

	for i := 0; i < numWaiters; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			m.Lock()
			cv.Wait(m)
			m.Unlock()

			mu.Lock()
			signaled++
			mu.Unlock()
		}()
	}

	time.Sleep(100 * time.Millisecond)

	cv.Broadcast()

	wg.Wait()

	if signaled != numWaiters {
		t.Errorf("Expected %d signaled, got %d", numWaiters, signaled)
	}
}

func TestCondVarWaitTimeout(t *testing.T) {
	m := NewMutex()
	cv := NewCondVar()

	m.Lock()

	start := time.Now()
	result := cv.WaitTimeout(m, 100*time.Millisecond)
	elapsed := time.Since(start)

	m.Unlock()

	if result {
		t.Error("WaitTimeout should return false on timeout")
	}

	if elapsed < 90*time.Millisecond {
		t.Errorf("Timeout too short: %v", elapsed)
	}
}

func TestCondVarProducerConsumer(t *testing.T) {
	m := NewMutex()
	cv := NewCondVar()

	queue := make([]int, 0)

	// Producer
	go func() {
		for i := 0; i < 5; i++ {
			time.Sleep(20 * time.Millisecond)

			m.Lock()
			queue = append(queue, i)
			m.Unlock()

			cv.Signal()
		}
	}()

	// Consumer
	consumed := make([]int, 0)
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()

		for i := 0; i < 5; i++ {
			m.Lock()

			for len(queue) == 0 {
				cv.Wait(m)
			}

			item := queue[0]
			queue = queue[1:]
			consumed = append(consumed, item)

			m.Unlock()
		}
	}()

	wg.Wait()

	if len(consumed) != 5 {
		t.Errorf("Expected 5 items consumed, got %d", len(consumed))
	}
}

func TestCondVarStats(t *testing.T) {
	m := NewMutex()
	cv := NewCondVar()

	go func() {
		m.Lock()
		cv.Wait(m)
		m.Unlock()
	}()

	time.Sleep(50 * time.Millisecond)

	cv.Signal()

	time.Sleep(50 * time.Millisecond)

	stats := cv.GetStats()

	if stats.Waits != 1 {
		t.Errorf("Expected 1 wait, got %d", stats.Waits)
	}

	if stats.Signals != 1 {
		t.Errorf("Expected 1 signal, got %d", stats.Signals)
	}
}

// TestMutexLockContextCancelled verifies that LockContext returns ctx.Err()
// when context is pre-cancelled and the mutex is already held.
func TestMutexLockContextCancelled(t *testing.T) {
	m := NewMutex()
	m.Lock() // hold it
	defer m.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := m.LockContext(ctx)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// TestMutexLockContextSuccess verifies that LockContext succeeds on an unlocked mutex.
func TestMutexLockContextSuccess(t *testing.T) {
	m := NewMutex()
	ctx := context.Background()
	if err := m.LockContext(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m.Unlock()
}

// TestCondVarWaitForBasic exercises the WaitFor helper.
func TestCondVarWaitForBasic(t *testing.T) {
	m := NewMutex()
	cv := NewCondVar()
	ready := false

	m.Lock()
	go func() {
		time.Sleep(20 * time.Millisecond)
		m.Lock()
		ready = true
		cv.Signal()
		m.Unlock()
	}()

	cv.WaitFor(m, func() bool { return ready })
	m.Unlock()

	if !ready {
		t.Error("WaitFor returned before condition was true")
	}
}

// TestMutexStringMethod exercises String() for coverage.
func TestMutexStringMethod(t *testing.T) {
	m := NewMutex()
	s := m.GetStats().String()
	if s == "" {
		t.Error("expected non-empty String()")
	}
}

// TestCondVarStringMethod exercises CondVarStats.String() for coverage.
func TestCondVarStringMethod(t *testing.T) {
	cv := NewCondVar()
	s := cv.GetStats().String()
	if s == "" {
		t.Error("expected non-empty String()")
	}
}
