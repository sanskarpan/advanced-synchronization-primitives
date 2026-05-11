package primitives

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestFairRWLockBasic(t *testing.T) {
	rw := NewFairRWLock()

	rw.RLock()
	rw.RUnlock() //lint:ignore SA2001 intentional: testing read lock/unlock round-trip

	rw.Lock()
	rw.Unlock() //lint:ignore SA2001 intentional: testing write lock/unlock round-trip
}

func TestFairRWLockFIFOOrdering(t *testing.T) {
	rw := NewFairRWLock()

	// Reader A acquires first.
	rw.RLock()

	writerAttempted := make(chan struct{})
	writerAcquired := make(chan struct{})
	releaseWriter := make(chan struct{})
	readerBAcquired := make(chan struct{})

	go func() {
		close(writerAttempted)
		rw.Lock()
		close(writerAcquired)
		<-releaseWriter
		rw.Unlock()
	}()

	<-writerAttempted
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if rw.GetStats().WriterWaits > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if rw.GetStats().WriterWaits == 0 {
		t.Fatal("writer was expected to be waiting")
	}

	go func() {
		rw.RLock()
		close(readerBAcquired)
		rw.RUnlock()
	}()

	// Reader B must not pass waiting writer while Reader A is still active.
	select {
	case <-readerBAcquired:
		t.Fatal("reader B should not acquire before writer in FIFO mode")
	case <-time.After(20 * time.Millisecond):
	}

	// Releasing Reader A should let queued writer acquire before Reader B.
	rw.RUnlock()

	select {
	case <-writerAcquired:
		// expected
	case <-time.After(500 * time.Millisecond):
		t.Fatal("writer did not acquire after last reader released")
	}

	select {
	case <-readerBAcquired:
		t.Fatal("reader B should still be blocked while writer holds lock")
	case <-time.After(20 * time.Millisecond):
	}

	close(releaseWriter)

	select {
	case <-readerBAcquired:
		// expected
	case <-time.After(500 * time.Millisecond):
		t.Fatal("reader B did not acquire after writer released")
	}
}

func TestFairRWLockMultipleReaders(t *testing.T) {
	rw := NewFairRWLock()
	const readers = 6

	var wg sync.WaitGroup
	wg.Add(readers)
	for i := 0; i < readers; i++ {
		go func() {
			defer wg.Done()
			rw.RLock()
			time.Sleep(5 * time.Millisecond)
			rw.RUnlock()
		}()
	}
	wg.Wait()

	stats := rw.GetStats()
	if stats.ReaderAcquires != readers {
		t.Fatalf("expected %d reader acquires, got %d", readers, stats.ReaderAcquires)
	}
}

func TestFairRWLockTryMethodsRespectQueue(t *testing.T) {
	rw := NewFairRWLock()
	rw.RLock()

	writerQueued := make(chan struct{})
	writerRelease := make(chan struct{})
	writerDone := make(chan struct{})

	go func() {
		close(writerQueued)
		rw.Lock()
		<-writerRelease
		rw.Unlock()
		close(writerDone)
	}()

	<-writerQueued
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if rw.GetStats().WriterWaits > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}

	if rw.TryRLock() {
		t.Fatal("TryRLock should fail when queue is non-empty")
	}
	if rw.TryLock() {
		t.Fatal("TryLock should fail when queue is non-empty")
	}

	rw.RUnlock()
	close(writerRelease)
	select {
	case <-writerDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("writer did not finish")
	}
}

func TestFairRWLockContextCancelled(t *testing.T) {
	rw := NewFairRWLock()
	rw.Lock()
	defer rw.Unlock()

	ctx1, cancel1 := context.WithCancel(context.Background())
	cancel1()
	if err := rw.RLockContext(ctx1); err == nil {
		t.Fatal("expected RLockContext to fail when context is cancelled")
	}

	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if err := rw.LockContext(ctx2); err == nil {
		t.Fatal("expected LockContext to fail when context is cancelled")
	}
}

func TestFairRWLockNoStarvationUnderMixedLoad(t *testing.T) {
	rw := NewFairRWLock()
	const workers = 24

	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		i := i
		go func() {
			defer wg.Done()
			if i%2 == 0 {
				rw.RLock()
				time.Sleep(time.Millisecond)
				rw.RUnlock()
				return
			}
			rw.Lock()
			time.Sleep(time.Millisecond)
			rw.Unlock()
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
		t.Fatal("mixed reader/writer workload did not complete (possible starvation)")
	}
}

func TestFairRWLockStatsPolicy(t *testing.T) {
	rw := NewFairRWLock()
	stats := rw.GetStats()
	if stats.Policy != "fair-fifo" {
		t.Fatalf("expected Policy fair-fifo, got %q", stats.Policy)
	}
	if stats.String() == "" {
		t.Fatal("expected non-empty stats string")
	}
}

func BenchmarkFairRWLockRead(b *testing.B) {
	rw := NewFairRWLock()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			rw.RLock()
			rw.RUnlock()
		}
	})
}

func BenchmarkFairRWLockWrite(b *testing.B) {
	rw := NewFairRWLock()
	for i := 0; i < b.N; i++ {
		rw.Lock()
		rw.Unlock()
	}
}

func BenchmarkRWLockVsFairRWLockReadHeavy(b *testing.B) {
	b.Run("RWLock", func(b *testing.B) {
		rw := NewRWLock()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				rw.RLock()
				rw.RUnlock()
			}
		})
	})

	b.Run("FairRWLock", func(b *testing.B) {
		rw := NewFairRWLock()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				rw.RLock()
				rw.RUnlock()
			}
		})
	})
}
