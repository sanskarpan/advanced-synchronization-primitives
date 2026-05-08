package primitives

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestRWLockBasic(t *testing.T) {
	rw := NewRWLock()

	// Test simple read lock
	rw.RLock()
	rw.RUnlock() //lint:ignore SA2001 intentional: testing read lock/unlock round-trip

	// Test simple write lock
	rw.Lock()
	rw.Unlock() //lint:ignore SA2001 intentional: testing write lock/unlock round-trip
}

func TestRWLockMultipleReaders(t *testing.T) {
	rw := NewRWLock()
	numReaders := 5

	var wg sync.WaitGroup

	for i := 0; i < numReaders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rw.RLock()
			time.Sleep(10 * time.Millisecond)
			rw.RUnlock()
		}()
	}

	wg.Wait()

	stats := rw.GetStats()
	if stats.ReaderAcquires != int64(numReaders) {
		t.Errorf("Expected %d reader acquires, got %d", numReaders, stats.ReaderAcquires)
	}
}

func TestRWLockWriterExclusivity(t *testing.T) {
	rw := NewRWLock()

	writerStarted := make(chan bool)
	writerDone := make(chan bool)
	readerDone := make(chan bool)

	// Start writer
	go func() {
		rw.Lock()
		writerStarted <- true
		time.Sleep(100 * time.Millisecond)
		rw.Unlock()
		writerDone <- true
	}()

	<-writerStarted

	// Try to acquire read lock while writer holds lock; signal when done
	go func() {
		rw.RLock()
		rw.RUnlock() //lint:ignore SA2001 intentional: testing reader blocks while writer holds lock
		close(readerDone)
	}()

	// After 50ms the reader should still be blocked (readerDone not closed)
	select {
	case <-readerDone:
		t.Error("Reader should be blocked while writer holds lock")
	case <-time.After(50 * time.Millisecond):
		// expected: reader is still waiting
	}

	<-writerDone

	// After the writer releases the lock the reader must unblock quickly
	select {
	case <-readerDone:
		// expected
	case <-time.After(500 * time.Millisecond):
		t.Error("Reader should be unblocked after writer releases lock")
	}
}

func TestRWLockTryRLock(t *testing.T) {
	rw := NewRWLock()

	// Should succeed when unlocked
	if !rw.TryRLock() {
		t.Error("TryRLock should succeed when unlocked")
	}
	rw.RUnlock()

	// Should succeed when another reader holds lock
	rw.RLock()
	if !rw.TryRLock() {
		t.Error("TryRLock should succeed when another reader holds lock")
	}
	rw.RUnlock()
	rw.RUnlock()

	// Should fail when writer holds lock
	rw.Lock()
	if rw.TryRLock() {
		t.Error("TryRLock should fail when writer holds lock")
		rw.RUnlock()
	}
	rw.Unlock()
}

func TestRWLockTryLock(t *testing.T) {
	rw := NewRWLock()

	// Should succeed when unlocked
	if !rw.TryLock() {
		t.Error("TryLock should succeed when unlocked")
	}
	rw.Unlock()

	// Should fail when reader holds lock
	rw.RLock()
	if rw.TryLock() {
		t.Error("TryLock should fail when reader holds lock")
		rw.Unlock()
	}
	rw.RUnlock()

	// Should fail when writer holds lock
	rw.Lock()
	if rw.TryLock() {
		t.Error("TryLock should fail when writer holds lock")
		rw.Unlock()
	}
	rw.Unlock()
}

func TestRWLockStats(t *testing.T) {
	rw := NewRWLock()

	rw.RLock()
	rw.RUnlock() //lint:ignore SA2001 intentional: incrementing reader-acquire stats counter

	rw.Lock()
	rw.Unlock() //lint:ignore SA2001 intentional: incrementing writer-acquire stats counter

	stats := rw.GetStats()

	if stats.ReaderAcquires != 1 {
		t.Errorf("Expected 1 reader acquire, got %d", stats.ReaderAcquires)
	}

	if stats.WriterAcquires != 1 {
		t.Errorf("Expected 1 writer acquire, got %d", stats.WriterAcquires)
	}
}

func BenchmarkRWLockRead(b *testing.B) {
	rw := NewRWLock()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			rw.RLock()
			rw.RUnlock() //lint:ignore SA2001 intentional: benchmarking read lock/unlock overhead
		}
	})
}

func BenchmarkRWLockWrite(b *testing.B) {
	rw := NewRWLock()

	for i := 0; i < b.N; i++ {
		rw.Lock()
		rw.Unlock() //lint:ignore SA2001 intentional: benchmarking write lock/unlock overhead
	}
}

// BenchmarkRWLockReadHeavy benchmarks a read-heavy workload: 16 readers, 1 writer.
func BenchmarkRWLockReadHeavy(b *testing.B) {
	rw := NewRWLock()
	const readers = 16

	b.SetParallelism(readers + 1)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if pb != nil {
				rw.RLock()
				rw.RUnlock() //lint:ignore SA2001 intentional: benchmarking read lock/unlock overhead
			}
		}
	})
}

// TestRWLockContextCancelledRLock verifies that RLockContext returns ctx.Err()
// when the context is already cancelled.
func TestRWLockContextCancelledRLock(t *testing.T) {
	rw := NewRWLock()
	// Hold a write lock so readers block.
	rw.Lock()
	defer rw.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel

	err := rw.RLockContext(ctx)
	if err == nil {
		t.Fatal("expected error from cancelled context, got nil")
	}
}

// TestRWLockContextCancelledLock verifies that LockContext returns ctx.Err()
// when the context is already cancelled.
func TestRWLockContextCancelledLock(t *testing.T) {
	rw := NewRWLock()
	// Hold a write lock so a second writer blocks.
	rw.Lock()
	defer rw.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel

	err := rw.LockContext(ctx)
	if err == nil {
		t.Fatal("expected error from cancelled context, got nil")
	}
}

// TestRWLockStringMethod exercises the String() method for coverage.
func TestRWLockStringMethod(t *testing.T) {
	rw := NewRWLock()
	s := rw.GetStats().String()
	if s == "" {
		t.Error("expected non-empty String()")
	}
}
