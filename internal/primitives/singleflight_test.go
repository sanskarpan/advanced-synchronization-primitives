package primitives

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSingleflightBasic(t *testing.T) {
	g := NewGroup()
	result, err := g.Do("key1", func() (interface{}, error) {
		return 42, nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Val != 42 {
		t.Errorf("expected 42, got %v", result.Val)
	}
	if result.Shared {
		t.Error("first call should not be shared")
	}
}

func TestSingleflightDeduplication(t *testing.T) {
	const goroutines = 20
	g := NewGroup()
	var callCount atomic.Int32
	var wg sync.WaitGroup

	// Release gate to make all goroutines hit Do concurrently.
	start := make(chan struct{})
	results := make([]Result, goroutines)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			r, _ := g.Do("shared", func() (interface{}, error) {
				callCount.Add(1)
				time.Sleep(20 * time.Millisecond) // hold to force sharing
				return "hello", nil
			})
			results[idx] = r
		}(i)
	}

	close(start) // release all goroutines simultaneously
	wg.Wait()

	// fn should have been called far fewer times than goroutines (ideally 1,
	// but races mean it could be a few; it must be << goroutines).
	if calls := callCount.Load(); calls > int32(goroutines/2) {
		t.Errorf("too many fn calls (%d); singleflight not deduplicating properly", calls)
	}

	// All results should have the same value.
	for i, r := range results {
		if r.Val != "hello" {
			t.Errorf("results[%d].Val = %v, want 'hello'", i, r.Val)
		}
	}
}

func TestSingleflightError(t *testing.T) {
	g := NewGroup()
	wantErr := errors.New("oops")
	r, err := g.Do("err-key", func() (interface{}, error) {
		return nil, wantErr
	})
	if err != wantErr {
		t.Errorf("expected wantErr, got %v", err)
	}
	if r.Err != wantErr {
		t.Errorf("r.Err: expected wantErr, got %v", r.Err)
	}
}

func TestSingleflightForget(t *testing.T) {
	g := NewGroup()
	var callCount atomic.Int32

	// Start a long-running call.
	block := make(chan struct{})
	done := make(chan struct{})
	go func() {
		g.Do("forget-key", func() (interface{}, error) {
			callCount.Add(1)
			<-block
			return nil, nil
		})
		close(done)
	}()

	// Give it a moment to register.
	time.Sleep(10 * time.Millisecond)

	// Forget the key so subsequent callers don't share.
	g.Forget("forget-key")

	// This call should run independently.
	called := false
	g.Do("forget-key", func() (interface{}, error) {
		called = true
		return nil, nil
	})

	if !called {
		t.Error("expected second fn to be called after Forget")
	}

	close(block) // unblock original goroutine
	<-done
}

func TestSingleflightStats(t *testing.T) {
	g := NewGroup()
	g.Do("k1", func() (interface{}, error) { return 1, nil })
	g.Do("k2", func() (interface{}, error) { return 2, nil })

	s := g.GetStats()
	if s.TotalCalls < 2 {
		t.Errorf("expected TotalCalls >= 2, got %d", s.TotalCalls)
	}
}

func TestSingleflightDoChan(t *testing.T) {
	g := NewGroup()
	ch := g.DoChan("chan-key", func() (interface{}, error) {
		return "result", nil
	})
	select {
	case r := <-ch:
		if r.Val != "result" {
			t.Errorf("expected 'result', got %v", r.Val)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("DoChan timed out")
	}
}
