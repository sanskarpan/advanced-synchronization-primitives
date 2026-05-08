package primitives

import (
	"sync"
	"sync/atomic"
	"time"
)

// OnceStats contains statistics about a Once.
type OnceStats struct {
	Done      bool
	DoCalls   int64
	ResetCalls int64
	Age       time.Duration
}

// Once allows a function to be executed exactly once, with a Reset method
// that allows reuse (unlike stdlib sync.Once).
type Once struct {
	mu        sync.Mutex
	done      atomic.Uint32 // 0 = not done, 1 = done
	doCalls   atomic.Int64
	resetCalls atomic.Int64
	createdAt time.Time
}

// NewOnce creates a new Once.
func NewOnce() *Once {
	return &Once{createdAt: time.Now()}
}

// Do calls f if and only if Do is being called for the first time since
// creation (or the last Reset). If Do is called multiple times only the
// first invocation calls f.
func (o *Once) Do(f func()) {
	// Fast path — already done.
	if o.done.Load() == 1 {
		return
	}

	o.mu.Lock()
	defer o.mu.Unlock()

	if o.done.Load() == 0 {
		defer o.done.Store(1)
		o.doCalls.Add(1)
		f()
	}
}

// Reset clears the done state so that a future Do call will execute its
// function again.
func (o *Once) Reset() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.done.Store(0)
	o.resetCalls.Add(1)
}

// Done returns true if Do has been called at least once since creation or
// the last Reset.
func (o *Once) Done() bool {
	return o.done.Load() == 1
}

// GetStats returns statistics about the Once.
func (o *Once) GetStats() OnceStats {
	return OnceStats{
		Done:       o.done.Load() == 1,
		DoCalls:    o.doCalls.Load(),
		ResetCalls: o.resetCalls.Load(),
		Age:        time.Since(o.createdAt),
	}
}
