package primitives

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestSemaphoreBasic(t *testing.T) {
	sem := NewSemaphore(1)

	sem.Acquire()
	if err := sem.Release(); err != nil {
		t.Fatalf("Release failed: %v", err)
	}

	if sem.GetCount() != 1 {
		t.Errorf("Expected count 1, got %d", sem.GetCount())
	}
}

func TestSemaphoreCapacity(t *testing.T) {
	capacity := int32(3)
	sem := NewSemaphore(capacity)

	if sem.GetCapacity() != capacity {
		t.Errorf("Expected capacity %d, got %d", capacity, sem.GetCapacity())
	}

	if sem.GetCount() != capacity {
		t.Errorf("Expected initial count %d, got %d", capacity, sem.GetCount())
	}
}

func TestSemaphoreAcquireRelease(t *testing.T) {
	sem := NewSemaphore(2)

	sem.Acquire()
	if sem.GetCount() != 1 {
		t.Errorf("Expected count 1 after acquire, got %d", sem.GetCount())
	}

	sem.Acquire()
	if sem.GetCount() != 0 {
		t.Errorf("Expected count 0 after second acquire, got %d", sem.GetCount())
	}

	if err := sem.Release(); err != nil {
		t.Fatalf("Release failed: %v", err)
	}
	if sem.GetCount() != 1 {
		t.Errorf("Expected count 1 after release, got %d", sem.GetCount())
	}

	if err := sem.Release(); err != nil {
		t.Fatalf("Release failed: %v", err)
	}
	if sem.GetCount() != 2 {
		t.Errorf("Expected count 2 after second release, got %d", sem.GetCount())
	}
}

func TestSemaphoreReleaseExceedsCapacity(t *testing.T) {
	sem := NewSemaphore(1)
	if err := sem.Release(); err == nil {
		t.Error("Release on full semaphore should return error")
	}
}

func TestSemaphoreAcquireN(t *testing.T) {
	sem := NewSemaphore(5)

	sem.AcquireN(3)
	if sem.GetCount() != 2 {
		t.Errorf("Expected count 2, got %d", sem.GetCount())
	}

	if err := sem.ReleaseN(3); err != nil {
		t.Fatalf("ReleaseN failed: %v", err)
	}
	if sem.GetCount() != 5 {
		t.Errorf("Expected count 5, got %d", sem.GetCount())
	}
}

func TestSemaphoreTryAcquire(t *testing.T) {
	sem := NewSemaphore(1)

	// Should succeed
	if !sem.TryAcquire() {
		t.Error("TryAcquire should succeed")
	}

	// Should fail
	if sem.TryAcquire() {
		t.Error("TryAcquire should fail when full")
	}

	if err := sem.Release(); err != nil {
		t.Fatalf("Release failed: %v", err)
	}

	// Should succeed again
	if !sem.TryAcquire() {
		t.Error("TryAcquire should succeed after release")
	}

	if err := sem.Release(); err != nil {
		t.Fatalf("Release failed: %v", err)
	}
}

func TestSemaphoreAcquireTimeout(t *testing.T) {
	sem := NewSemaphore(1)
	sem.Acquire()

	start := time.Now()
	result := sem.AcquireTimeout(100 * time.Millisecond)
	elapsed := time.Since(start)

	if result {
		t.Error("AcquireTimeout should fail")
	}

	if elapsed < 90*time.Millisecond {
		t.Errorf("Timeout too short: %v", elapsed)
	}

	if err := sem.Release(); err != nil {
		t.Fatalf("Release failed: %v", err)
	}

	if !sem.AcquireTimeout(100 * time.Millisecond) {
		t.Error("AcquireTimeout should succeed when available")
	}

	if err := sem.Release(); err != nil {
		t.Fatalf("Release failed: %v", err)
	}
}

func TestSemaphoreConcurrency(t *testing.T) {
	sem := NewSemaphore(3)
	numWorkers := 10

	var wg sync.WaitGroup
	var mu sync.Mutex
	active := int32(0)
	maxActive := int32(0)

	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			sem.Acquire()

			mu.Lock()
			active++
			current := active
			if current > maxActive {
				maxActive = current
			}
			mu.Unlock()

			time.Sleep(10 * time.Millisecond)

			mu.Lock()
			active--
			mu.Unlock()

			if err := sem.Release(); err != nil {
				t.Errorf("Release failed: %v", err)
			}
		}()
	}

	wg.Wait()

	if maxActive > 3 {
		t.Errorf("Max active should be <= 3, got %d", maxActive)
	}

	stats := sem.GetStats()
	if stats.Acquires != int64(numWorkers) {
		t.Errorf("Expected %d acquires, got %d", numWorkers, stats.Acquires)
	}
}

func TestSemaphoreStats(t *testing.T) {
	sem := NewSemaphore(2)

	sem.Acquire()
	if err := sem.Release(); err != nil {
		t.Fatalf("Release failed: %v", err)
	}

	stats := sem.GetStats()

	if stats.Acquires != 1 {
		t.Errorf("Expected 1 acquire, got %d", stats.Acquires)
	}

	if stats.Releases != 1 {
		t.Errorf("Expected 1 release, got %d", stats.Releases)
	}
}

func BenchmarkSemaphoreAcquireRelease(b *testing.B) {
	sem := NewSemaphore(10)

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			sem.Acquire()
			_ = sem.Release()
		}
	})
}

// BenchmarkSemaphoreBurst benchmarks concurrent burst acquires and releases.
func BenchmarkSemaphoreBurst(b *testing.B) {
	const capacity = 32
	sem := NewSemaphore(capacity)

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			sem.Acquire()
			_ = sem.Release()
		}
	})
}

// TestSemaphoreAcquireContextSuccess verifies AcquireContext succeeds immediately.
func TestSemaphoreAcquireContextSuccess(t *testing.T) {
	sem := NewSemaphore(2)
	ctx := context.Background()
	if err := sem.AcquireContext(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sem.Release()
}

// TestSemaphoreAcquireContextCancelled verifies AcquireContext returns ctx.Err()
// when the semaphore is exhausted and context is pre-cancelled.
func TestSemaphoreAcquireContextCancelled(t *testing.T) {
	sem := NewSemaphore(1)
	sem.Acquire() // exhaust
	defer sem.Release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := sem.AcquireContext(ctx)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// TestSemaphoreAcquireNContextSuccess verifies AcquireNContext succeeds when enough slots.
func TestSemaphoreAcquireNContextSuccess(t *testing.T) {
	sem := NewSemaphore(3)
	ctx := context.Background()
	if err := sem.AcquireNContext(ctx, 2); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sem.ReleaseN(2)
}

// TestSemaphoreStringMethod exercises String() for coverage.
func TestSemaphoreStringMethod(t *testing.T) {
	sem := NewSemaphore(5)
	s := sem.GetStats().String()
	if s == "" {
		t.Error("expected non-empty String()")
	}
}
