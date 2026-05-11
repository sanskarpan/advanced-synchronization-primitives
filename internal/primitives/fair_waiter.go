package primitives

import (
	"context"
	"sync/atomic"
	"time"
)

type fairWaiterKind uint8

const (
	fairWaiterKindReader fairWaiterKind = iota
	fairWaiterKindWriter
)

type fairWaiter struct {
	ID        uint64
	kind      fairWaiterKind
	Ready     chan struct{}
	CreatedAt time.Time
	signaled  atomic.Bool
	cancelled atomic.Bool
	next      atomic.Pointer[fairWaiter]
}

func newFairWaiter(kind fairWaiterKind) *fairWaiter {
	return &fairWaiter{
		ID:        waiterIDCounter.Add(1),
		kind:      kind,
		Ready:     make(chan struct{}, 1),
		CreatedAt: time.Now(),
	}
}

func (w *fairWaiter) signal() {
	w.signaled.Store(true)
	select {
	case w.Ready <- struct{}{}:
	default:
	}
}

func (w *fairWaiter) wait() {
	<-w.Ready
}

func (w *fairWaiter) waitContext(ctx context.Context) error {
	select {
	case <-w.Ready:
		return nil
	case <-ctx.Done():
		w.cancelled.Store(true)
		return ctx.Err()
	}
}
