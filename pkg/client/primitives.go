package client

import "context"

func (c *Client) create(ctx context.Context, msgType string, payload interface{}) error {
	_, err := c.send(ctx, msgType, payload)
	return err
}

func (c *Client) op(ctx context.Context, id, op string, holdMs int) error {
	payload := map[string]interface{}{
		"id": id,
		"op": op,
	}
	if holdMs != 0 {
		payload["holdMs"] = holdMs
	}
	_, err := c.send(ctx, "primitiveOp", payload)
	return err
}

// CreateRWLock creates an RWLock primitive.
func (c *Client) CreateRWLock(ctx context.Context, id, name string) error {
	return c.create(ctx, "createRWLock", map[string]interface{}{"id": id, "name": name})
}

// RLockRWLock acquires a read lock on an RWLock.
func (c *Client) RLockRWLock(ctx context.Context, id string, holdMs int) error {
	return c.op(ctx, id, "rlock", holdMs)
}

// RUnlockRWLock releases a read lock on an RWLock.
func (c *Client) RUnlockRWLock(ctx context.Context, id string) error {
	return c.op(ctx, id, "runlock", 0)
}

// LockRWLock acquires a write lock on an RWLock.
func (c *Client) LockRWLock(ctx context.Context, id string, holdMs int) error {
	return c.op(ctx, id, "lock", holdMs)
}

// UnlockRWLock releases a write lock on an RWLock.
func (c *Client) UnlockRWLock(ctx context.Context, id string) error {
	return c.op(ctx, id, "unlock", 0)
}

// TryRLockRWLock attempts a non-blocking read lock operation on an RWLock.
func (c *Client) TryRLockRWLock(ctx context.Context, id string) error {
	return c.op(ctx, id, "tryRLock", 0)
}

// TryLockRWLock attempts a non-blocking write lock operation on an RWLock.
func (c *Client) TryLockRWLock(ctx context.Context, id string) error {
	return c.op(ctx, id, "tryLock", 0)
}

// CreateSemaphore creates a semaphore primitive.
func (c *Client) CreateSemaphore(ctx context.Context, id, name string, capacity int32) error {
	return c.create(ctx, "createSemaphore", map[string]interface{}{
		"id":       id,
		"name":     name,
		"capacity": capacity,
	})
}

// AcquireSemaphore acquires one slot from a semaphore.
func (c *Client) AcquireSemaphore(ctx context.Context, id string, holdMs int) error {
	return c.op(ctx, id, "acquire", holdMs)
}

// ReleaseSemaphore releases one slot back to a semaphore.
func (c *Client) ReleaseSemaphore(ctx context.Context, id string) error {
	return c.op(ctx, id, "release", 0)
}

// CreateMutex creates a mutex primitive.
func (c *Client) CreateMutex(ctx context.Context, id, name string) error {
	return c.create(ctx, "createMutex", map[string]interface{}{"id": id, "name": name})
}

// LockMutex acquires a mutex.
func (c *Client) LockMutex(ctx context.Context, id string, holdMs int) error {
	return c.op(ctx, id, "lock", holdMs)
}

// UnlockMutex releases a mutex.
func (c *Client) UnlockMutex(ctx context.Context, id string) error {
	return c.op(ctx, id, "unlock", 0)
}

// TryLockMutex attempts a non-blocking mutex lock operation.
func (c *Client) TryLockMutex(ctx context.Context, id string) error {
	return c.op(ctx, id, "tryLock", 0)
}

// CreateCondVar creates a condition variable primitive.
func (c *Client) CreateCondVar(ctx context.Context, id, name string) error {
	return c.create(ctx, "createCondVar", map[string]interface{}{"id": id, "name": name})
}

// WaitCondVar waits on a condition variable.
func (c *Client) WaitCondVar(ctx context.Context, id string, holdMs int) error {
	return c.op(ctx, id, "wait", holdMs)
}

// SignalCondVar wakes one waiter on a condition variable.
func (c *Client) SignalCondVar(ctx context.Context, id string) error {
	return c.op(ctx, id, "signal", 0)
}

// BroadcastCondVar wakes all waiters on a condition variable.
func (c *Client) BroadcastCondVar(ctx context.Context, id string) error {
	return c.op(ctx, id, "broadcast", 0)
}

// CreateBarrier creates a barrier primitive.
func (c *Client) CreateBarrier(ctx context.Context, id, name string, parties int32) error {
	return c.create(ctx, "createBarrier", map[string]interface{}{
		"id":      id,
		"name":    name,
		"parties": parties,
	})
}

// WaitBarrier waits for a barrier to trip.
func (c *Client) WaitBarrier(ctx context.Context, id string) error {
	return c.op(ctx, id, "wait", 0)
}

// ResetBarrier resets a barrier.
func (c *Client) ResetBarrier(ctx context.Context, id string) error {
	return c.op(ctx, id, "reset", 0)
}

// CreateWaitGroup creates a wait group primitive.
func (c *Client) CreateWaitGroup(ctx context.Context, id, name string) error {
	return c.create(ctx, "createWaitGroup", map[string]interface{}{"id": id, "name": name})
}

// AddWaitGroup adds delta to a wait group.
func (c *Client) AddWaitGroup(ctx context.Context, id string, delta int) error {
	return c.op(ctx, id, "add", delta)
}

// DoneWaitGroup decrements a wait group.
func (c *Client) DoneWaitGroup(ctx context.Context, id string) error {
	return c.op(ctx, id, "done", 0)
}

// WaitWaitGroup waits for a wait group to reach zero.
func (c *Client) WaitWaitGroup(ctx context.Context, id string) error {
	return c.op(ctx, id, "wg-wait", 0)
}

// CreateOnce creates a once primitive.
func (c *Client) CreateOnce(ctx context.Context, id, name string) error {
	return c.create(ctx, "createOnce", map[string]interface{}{"id": id, "name": name})
}

// DoOnce executes the once primitive.
func (c *Client) DoOnce(ctx context.Context, id string) error {
	return c.op(ctx, id, "do", 0)
}

// CreateSingleflight creates a singleflight primitive.
func (c *Client) CreateSingleflight(ctx context.Context, id, name string) error {
	return c.create(ctx, "createSingleflight", map[string]interface{}{"id": id, "name": name})
}

// DoSingleflight executes a singleflight operation.
func (c *Client) DoSingleflight(ctx context.Context, id string) error {
	return c.op(ctx, id, "do", 0)
}

// ForgetSingleflight clears a singleflight key.
func (c *Client) ForgetSingleflight(ctx context.Context, id string) error {
	return c.op(ctx, id, "forget", 0)
}

// DeletePrimitive deletes a primitive by ID.
func (c *Client) DeletePrimitive(ctx context.Context, id string) error {
	_, err := c.send(ctx, "deletePrimitive", map[string]interface{}{"id": id})
	return err
}
