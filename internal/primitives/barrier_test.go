package primitives

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestBarrierBasic(t *testing.T) {
	barrier := NewBarrier(3)

	if barrier.GetParties() != 3 {
		t.Errorf("Expected 3 parties, got %d", barrier.GetParties())
	}

	if barrier.GetArrived() != 0 {
		t.Errorf("Expected 0 arrived, got %d", barrier.GetArrived())
	}
}

func TestBarrierWait(t *testing.T) {
	// Single party barrier - simple test
	barrier := NewBarrier(1)

	arrivalIndex, err := barrier.Wait()
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if arrivalIndex != 0 {
		t.Errorf("Expected arrival index 0, got %d", arrivalIndex)
	}
}

func TestBarrierWaitTimeout(t *testing.T) {
	barrier := NewBarrier(2)

	start := time.Now()
	arrivalIndex, success := barrier.WaitTimeout(100 * time.Millisecond)
	elapsed := time.Since(start)

	if success {
		t.Error("WaitTimeout should fail")
	}

	if arrivalIndex != -1 {
		t.Errorf("Expected arrival index -1, got %d", arrivalIndex)
	}

	if elapsed < 90*time.Millisecond {
		t.Errorf("Timeout too short: %v", elapsed)
	}
}

func TestBarrierWaitTimeoutSuccess(t *testing.T) {
	barrier := NewBarrier(2)

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		barrier.Wait()
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()

		arrivalIndex, success := barrier.WaitTimeout(500 * time.Millisecond)

		if !success {
			t.Error("WaitTimeout should succeed")
		}

		if arrivalIndex < 0 || arrivalIndex >= 2 {
			t.Errorf("Invalid arrival index: %d", arrivalIndex)
		}
	}()

	wg.Wait()
}

func TestBarrierReset(t *testing.T) {
	barrier := NewBarrier(3)

	go func() {
		barrier.Wait()
	}()

	time.Sleep(50 * time.Millisecond)

	arrived := barrier.GetArrived()
	if arrived != 1 {
		t.Errorf("Expected 1 arrived, got %d", arrived)
	}

	barrier.Reset()

	if barrier.GetArrived() != 0 {
		t.Errorf("Expected 0 arrived after reset, got %d", barrier.GetArrived())
	}
}

func TestBarrierGeneration(t *testing.T) {
	barrier := NewBarrier(1)

	initialGen := barrier.GetGeneration()

	// Single party barrier trips immediately
	barrier.Wait()

	newGen := barrier.GetGeneration()

	if newGen != initialGen+1 {
		t.Errorf("Expected generation %d, got %d", initialGen+1, newGen)
	}
}

func TestBarrierStats(t *testing.T) {
	barrier := NewBarrier(1)

	// Single party barrier trips immediately
	barrier.Wait()

	stats := barrier.GetStats()

	if stats.Trips != 1 {
		t.Errorf("Expected 1 trip, got %d", stats.Trips)
	}

	if stats.Generation != 1 {
		t.Errorf("Expected generation 1, got %d", stats.Generation)
	}
}

func TestBarrierOverSubscription(t *testing.T) {
	// With parties=2, launch exactly parties goroutines concurrently.
	// They should trip the barrier and all return, arrived stays non-negative.
	b := NewBarrier(2)

	results := make(chan int32, 2)
	for i := 0; i < 2; i++ {
		go func() {
			idx, _ := b.Wait()
			results <- idx
		}()
	}

	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	collected := 0
	for collected < 2 {
		select {
		case <-results:
			collected++
		case <-timer.C:
			t.Fatalf("timed out waiting for goroutines, got %d/2 results", collected)
		}
	}

	if arr := b.GetArrived(); arr < 0 {
		t.Errorf("arrived went negative: %d", arr)
	}
}

func TestBarrierOverSubscriptionWaitTimeout(t *testing.T) {
	// With parties=2, all 2 goroutines use WaitTimeout; they should all return.
	b := NewBarrier(2)

	type result struct {
		idx int32
		ok  bool
	}
	results := make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func() {
			idx, ok := b.WaitTimeout(500 * time.Millisecond)
			results <- result{idx, ok}
		}()
	}

	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for i := 0; i < 2; i++ {
		select {
		case <-results:
		case <-timer.C:
			t.Fatalf("timed out after collecting %d/2 results", i)
		}
	}

	if arr := b.GetArrived(); arr < 0 {
		t.Errorf("arrived went negative: %d", arr)
	}
}

// TestBarrierBroken verifies that Break() wakes all waiters with ErrBarrierBroken.
func TestBarrierBroken(t *testing.T) {
	const parties = 3
	barrier := NewBarrier(parties)

	errs := make(chan error, parties-1)

	// Start parties-1 goroutines waiting at the barrier.
	for i := 0; i < parties-1; i++ {
		go func() {
			_, err := barrier.Wait()
			errs <- err
		}()
	}

	// Give goroutines time to reach Wait and block.
	time.Sleep(50 * time.Millisecond)

	// Break the barrier.
	barrier.Break()

	// All waiting goroutines should return ErrBarrierBroken.
	timeout := time.NewTimer(2 * time.Second)
	defer timeout.Stop()
	for i := 0; i < parties-1; i++ {
		select {
		case err := <-errs:
			if err != ErrBarrierBroken {
				t.Errorf("expected ErrBarrierBroken, got %v", err)
			}
		case <-timeout.C:
			t.Fatalf("timed out waiting for goroutine %d to return", i)
		}
	}

	if !barrier.IsBroken() {
		t.Error("barrier.IsBroken() should return true after Break()")
	}
}

// TestBarrierBrokenReset verifies that after breaking and resetting, the barrier works normally.
func TestBarrierBrokenReset(t *testing.T) {
	const parties = 2
	barrier := NewBarrier(parties)

	barrier.Break()
	if !barrier.IsBroken() {
		t.Fatal("barrier should be broken after Break()")
	}

	barrier.Reset()
	if barrier.IsBroken() {
		t.Fatal("barrier should not be broken after Reset()")
	}

	// Normal use after reset.
	done := make(chan struct{})
	go func() {
		barrier.Wait()
		close(done)
	}()
	barrier.Wait()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("barrier did not trip after reset")
	}
}

func BenchmarkBarrier(b *testing.B) {
	numParties := int32(4)
	barrier := NewBarrier(numParties)

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		var wg sync.WaitGroup

		for j := int32(0); j < numParties; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				barrier.Wait()
			}()
		}

		wg.Wait()
	}
}

// BenchmarkBarrierLarge benchmarks a barrier with 32 parties.
func BenchmarkBarrierLarge(b *testing.B) {
	const parties = int32(32)
	barrier := NewBarrier(parties)

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		var wg sync.WaitGroup
		for j := int32(0); j < parties; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				barrier.Wait()
			}()
		}
		wg.Wait()
	}
}

// TestBarrierWaitContext exercises WaitContext with a 2-party barrier.
func TestBarrierWaitContext(t *testing.T) {
	b := NewBarrier(2)

	done := make(chan struct{})
	go func() {
		_, err := b.WaitContext(context.Background())
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		close(done)
	}()

	// Let the goroutine enqueue.
	time.Sleep(10 * time.Millisecond)

	// Last party arrives — both should be released.
	idx, err := b.WaitContext(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if idx != 1 {
		t.Errorf("expected arrival index 1 (last party), got %d", idx)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("goroutine did not unblock after barrier trip")
	}
}

// TestBarrierWaitContextCancelled verifies WaitContext returns ctx.Err()
// when the context is cancelled while waiting.
func TestBarrierWaitContextCancelled(t *testing.T) {
	b := NewBarrier(3) // needs 3 parties; we only send 1

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		_, err := b.WaitContext(ctx)
		errCh <- err
	}()

	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected context error, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitContext did not return after cancel")
	}
}

// TestBarrierStringMethod exercises String() for coverage.
func TestBarrierStringMethod(t *testing.T) {
	b := NewBarrier(2)
	s := b.GetStats().String()
	if s == "" {
		t.Error("expected non-empty String()")
	}
}
