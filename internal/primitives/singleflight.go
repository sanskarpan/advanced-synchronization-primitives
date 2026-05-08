package primitives

import (
	"sync"
	"sync/atomic"
	"time"
)

// Result holds the result of a Singleflight Do call.
type Result struct {
	Val    interface{}
	Err    error
	Shared bool // true if the result was shared with concurrent callers
}

// call represents an in-flight or completed call.
type call struct {
	wg  sync.WaitGroup
	val interface{}
	err error
}

// SingleflightStats contains statistics about a Group.
type SingleflightStats struct {
	TotalCalls  int64
	SharedCalls int64
	Age         time.Duration
}

// Group deduplicates concurrent calls with the same key.
type Group struct {
	mu          sync.Mutex
	calls       map[string]*call
	totalCalls  atomic.Int64
	sharedCalls atomic.Int64
	createdAt   time.Time
}

// NewGroup creates a new Singleflight Group.
func NewGroup() *Group {
	return &Group{
		calls:     make(map[string]*call),
		createdAt: time.Now(),
	}
}

// Do executes fn if no call with the given key is currently in flight.
// Concurrent callers with the same key share the result.
func (g *Group) Do(key string, fn func() (interface{}, error)) (Result, error) {
	g.mu.Lock()
	if c, ok := g.calls[key]; ok {
		g.mu.Unlock()
		g.sharedCalls.Add(1)
		c.wg.Wait()
		return Result{Val: c.val, Err: c.err, Shared: true}, c.err
	}

	c := &call{}
	c.wg.Add(1)
	g.calls[key] = c
	g.mu.Unlock()

	g.totalCalls.Add(1)

	c.val, c.err = fn()
	c.wg.Done()

	g.mu.Lock()
	delete(g.calls, key)
	g.mu.Unlock()

	return Result{Val: c.val, Err: c.err, Shared: false}, c.err
}

// DoChan is like Do but returns a channel that receives the result.
func (g *Group) DoChan(key string, fn func() (interface{}, error)) <-chan Result {
	ch := make(chan Result, 1)
	go func() {
		r, _ := g.Do(key, fn)
		ch <- r
	}()
	return ch
}

// Forget removes the in-flight call for key, if any, so that subsequent
// callers will invoke fn fresh.
func (g *Group) Forget(key string) {
	g.mu.Lock()
	delete(g.calls, key)
	g.mu.Unlock()
}

// GetStats returns statistics about the Group.
func (g *Group) GetStats() SingleflightStats {
	return SingleflightStats{
		TotalCalls:  g.totalCalls.Load(),
		SharedCalls: g.sharedCalls.Load(),
		Age:         time.Since(g.createdAt),
	}
}
