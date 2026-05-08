package primitives

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestOnceBasic(t *testing.T) {
	o := NewOnce()
	if o.Done() {
		t.Error("Once should not be done initially")
	}

	called := 0
	o.Do(func() { called++ })
	if called != 1 {
		t.Errorf("expected 1 call, got %d", called)
	}
	if !o.Done() {
		t.Error("Once should be done after Do")
	}

	// Second call should be a no-op.
	o.Do(func() { called++ })
	if called != 1 {
		t.Errorf("expected still 1 call, got %d", called)
	}
}

func TestOnceConcurrent(t *testing.T) {
	const goroutines = 100
	o := NewOnce()
	var count atomic.Int32
	var wg sync.WaitGroup

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o.Do(func() { count.Add(1) })
		}()
	}
	wg.Wait()

	if count.Load() != 1 {
		t.Errorf("expected fn called exactly once, got %d", count.Load())
	}
}

func TestOnceReset(t *testing.T) {
	o := NewOnce()
	called := 0
	o.Do(func() { called++ })
	if called != 1 {
		t.Fatalf("expected 1, got %d", called)
	}

	o.Reset()
	if o.Done() {
		t.Error("Once should not be done after Reset")
	}

	o.Do(func() { called++ })
	if called != 2 {
		t.Errorf("expected 2 after reset+Do, got %d", called)
	}
}

func TestOnceStats(t *testing.T) {
	o := NewOnce()
	o.Do(func() {})
	o.Reset()
	o.Do(func() {})

	s := o.GetStats()
	if s.DoCalls != 2 {
		t.Errorf("expected DoCalls 2, got %d", s.DoCalls)
	}
	if s.ResetCalls != 1 {
		t.Errorf("expected ResetCalls 1, got %d", s.ResetCalls)
	}
	if !s.Done {
		t.Error("expected Done=true")
	}
}
