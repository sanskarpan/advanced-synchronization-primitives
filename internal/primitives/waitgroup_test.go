package primitives

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWaitGroupBasic(t *testing.T) {
	wg := NewWaitGroup()
	if wg.GetCount() != 0 {
		t.Fatalf("expected count 0, got %d", wg.GetCount())
	}

	wg.Add(3)
	if wg.GetCount() != 3 {
		t.Fatalf("expected count 3, got %d", wg.GetCount())
	}

	wg.Done()
	wg.Done()
	wg.Done()
	if wg.GetCount() != 0 {
		t.Fatalf("expected count 0 after 3 Dones, got %d", wg.GetCount())
	}
}

func TestWaitGroupWaitBlocks(t *testing.T) {
	wg := NewWaitGroup()
	wg.Add(1)

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	// Give goroutine time to reach Wait.
	time.Sleep(20 * time.Millisecond)

	select {
	case <-done:
		t.Fatal("Wait returned before Done was called")
	default:
	}

	wg.Done()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not unblock after Done")
	}
}

func TestWaitGroupConcurrent(t *testing.T) {
	const workers = 50
	wg := NewWaitGroup()
	var stdWG sync.WaitGroup
	counter := make(chan int, workers)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		stdWG.Add(1)
		go func(n int) {
			defer stdWG.Done()
			time.Sleep(time.Duration(n%5) * time.Millisecond)
			counter <- n
			wg.Done()
		}(i)
	}

	waitDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(waitDone)
	}()

	stdWG.Wait()
	select {
	case <-waitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("WaitGroup.Wait did not return after all Done calls")
	}

	if got := len(counter); got != workers {
		t.Errorf("expected %d items in channel, got %d", workers, got)
	}
}

func TestWaitGroupNegativePanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Error("expected panic on negative counter")
			return
		}
		msg := fmt.Sprint(r)
		if !strings.Contains(msg, "negative counter") ||
			!strings.Contains(msg, "Done") ||
			!strings.Contains(msg, "counter=-1") {
			t.Fatalf("unexpected panic message: %q", msg)
		}
	}()
	wg := NewWaitGroup()
	wg.Done() // counter = -1 → panic
}

func TestWaitGroupNegativeAddPanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on negative Add")
		}
		msg := fmt.Sprint(r)
		if !strings.Contains(msg, "negative counter") ||
			!strings.Contains(msg, "delta=-1") ||
			!strings.Contains(msg, "counter=-1") {
			t.Fatalf("unexpected panic message: %q", msg)
		}
	}()
	wg := NewWaitGroup()
	wg.Add(-1)
}

func TestWaitGroupWaitContext(t *testing.T) {
	wg := NewWaitGroup()
	wg.Add(1)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := wg.WaitContext(ctx)
	if err == nil {
		t.Error("expected context timeout error")
	}

	// After cancellation, Done should still work.
	wg.Done()
	if wg.GetCount() != 0 {
		t.Errorf("expected count 0, got %d", wg.GetCount())
	}
}

func TestWaitGroupStats(t *testing.T) {
	wg := NewWaitGroup()
	wg.Add(2)
	wg.Done()
	wg.Done()

	s := wg.GetStats()
	if s.Count != 0 {
		t.Errorf("expected Count 0, got %d", s.Count)
	}
	if s.DoneCount != 2 {
		t.Errorf("expected DoneCount 2, got %d", s.DoneCount)
	}
}
