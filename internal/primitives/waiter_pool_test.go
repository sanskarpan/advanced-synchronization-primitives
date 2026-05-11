package primitives

import (
	"sync"
	"testing"
)

func TestWaiterPoolResetOnGet(t *testing.T) {
	w := getWaiter()
	w.cancelled.Store(true)
	w.Signal()
	putWaiter(w)

	w2 := getWaiter()
	t.Cleanup(func() { putWaiter(w2) })

	if w2.cancelled.Load() {
		t.Fatal("expected cancelled flag to be reset")
	}

	select {
	case <-w2.Ready:
		t.Fatal("expected pooled waiter channel to be drained")
	default:
	}
}

func TestWaiterPoolConcurrentSafe(t *testing.T) {
	const workers = 128

	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			w := getWaiter()
			w.Signal()
			<-w.Ready
			putWaiter(w)
		}()
	}
	wg.Wait()
}
