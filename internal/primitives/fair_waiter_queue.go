package primitives

import "sync/atomic"

type fairWaiterQueue struct {
	head atomic.Pointer[fairWaiter]
	tail atomic.Pointer[fairWaiter]
	size atomic.Int64
}

func newFairWaiterQueue() *fairWaiterQueue {
	sentinel := &fairWaiter{}
	q := &fairWaiterQueue{}
	q.head.Store(sentinel)
	q.tail.Store(sentinel)
	return q
}

func (q *fairWaiterQueue) enqueue(w *fairWaiter) {
	w.next.Store(nil)
	for {
		tail := q.tail.Load()
		next := tail.next.Load()
		if tail != q.tail.Load() {
			continue
		}
		if next == nil {
			if tail.next.CompareAndSwap(nil, w) {
				q.tail.CompareAndSwap(tail, w)
				q.size.Add(1)
				return
			}
			continue
		}
		q.tail.CompareAndSwap(tail, next)
	}
}

func (q *fairWaiterQueue) dequeue() *fairWaiter {
	for {
		head := q.head.Load()
		tail := q.tail.Load()
		next := head.next.Load()
		if head != q.head.Load() {
			continue
		}
		if head == tail {
			if next == nil {
				return nil
			}
			q.tail.CompareAndSwap(tail, next)
			continue
		}
		item := next
		if q.head.CompareAndSwap(head, next) {
			q.size.Add(-1)
			if item.cancelled.Load() {
				continue
			}
			return item
		}
	}
}

func (q *fairWaiterQueue) peek() *fairWaiter {
	for {
		head := q.head.Load()
		tail := q.tail.Load()
		next := head.next.Load()
		if head != q.head.Load() {
			continue
		}
		if head == tail {
			if next == nil {
				return nil
			}
			q.tail.CompareAndSwap(tail, next)
			continue
		}
		if next.cancelled.Load() {
			if q.head.CompareAndSwap(head, next) {
				q.size.Add(-1)
			}
			continue
		}
		return next
	}
}

func (q *fairWaiterQueue) dequeueReaders() []*fairWaiter {
	first := q.dequeue()
	if first == nil {
		return nil
	}
	if first.kind != fairWaiterKindReader {
		return []*fairWaiter{first}
	}

	readers := []*fairWaiter{first}
	for {
		head := q.peek()
		if head == nil || head.kind != fairWaiterKindReader {
			return readers
		}
		next := q.dequeue()
		if next == nil {
			return readers
		}
		if next.kind != fairWaiterKindReader {
			return append(readers, next)
		}
		readers = append(readers, next)
	}
}

func (q *fairWaiterQueue) sizeValue() int64 {
	v := q.size.Load()
	if v < 0 {
		return 0
	}
	return v
}

func (q *fairWaiterQueue) isEmpty() bool {
	return q.sizeValue() == 0
}
