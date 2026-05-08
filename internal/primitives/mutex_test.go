package primitives

import (
	"sync"
	"testing"
)

func TestMutexBasic(t *testing.T) {
	m := NewMutex()

	m.Lock()
	m.Unlock() //lint:ignore SA2001 intentional: testing lock/unlock round-trip

	if m.IsLocked() {
		t.Error("Mutex should be unlocked")
	}
}

func TestMutexExclusivity(t *testing.T) {
	m := NewMutex()
	counter := 0

	var wg sync.WaitGroup
	numGoroutines := 10
	increments := 100

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			for j := 0; j < increments; j++ {
				m.Lock()
				counter++
				m.Unlock()
			}
		}()
	}

	wg.Wait()

	expected := numGoroutines * increments
	if counter != expected {
		t.Errorf("Expected counter %d, got %d", expected, counter)
	}
}

func TestMutexTryLock(t *testing.T) {
	m := NewMutex()

	// Should succeed
	if !m.TryLock() {
		t.Error("TryLock should succeed on unlocked mutex")
	}

	// Should fail
	if m.TryLock() {
		t.Error("TryLock should fail on locked mutex")
	}

	m.Unlock()

	// Should succeed again
	if !m.TryLock() {
		t.Error("TryLock should succeed after unlock")
	}

	m.Unlock()
}

func TestMutexStats(t *testing.T) {
	m := NewMutex()

	m.Lock()
	m.Unlock() //lint:ignore SA2001 intentional: incrementing stats counters

	m.Lock()
	m.Unlock() //lint:ignore SA2001 intentional: incrementing stats counters

	stats := m.GetStats()

	if stats.Locks != 2 {
		t.Errorf("Expected 2 locks, got %d", stats.Locks)
	}

	if stats.Unlocks != 2 {
		t.Errorf("Expected 2 unlocks, got %d", stats.Unlocks)
	}
}

func BenchmarkMutex(b *testing.B) {
	m := NewMutex()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			m.Lock()
			m.Unlock() //lint:ignore SA2001 intentional: benchmarking lock/unlock overhead
		}
	})
}

// BenchmarkMutexContended benchmarks the mutex under contention from 8 goroutines.
func BenchmarkMutexContended(b *testing.B) {
	m := NewMutex()
	const goroutines = 8

	b.SetParallelism(goroutines)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			m.Lock()
			m.Unlock() //lint:ignore SA2001 intentional: benchmarking lock/unlock overhead
		}
	})
}
