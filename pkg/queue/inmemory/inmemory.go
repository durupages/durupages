// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

// Package inmemory provides the default in-memory queue.Queue implementation:
// a strict per-tenant FIFO with blocking Dequeue. It assumes a single
// controller replica (controller replicas=1) and keeps all state in memory, so
// pending requests are lost on restart. A Redis-backed implementation can
// replace it to survive restarts or enable controller HA.
//
// Concurrency model: each tenant owns an independent FIFO guarded by its own
// mutex and condition variable. Every queued item carries a state that
// transitions from "queued" to exactly one of "delivered" or "cancelled" under
// that mutex, which is what makes delivery and cancellation mutually exclusive
// (see New for details).
package inmemory

import (
	"context"
	"sync"

	"github.com/durupages/durupages/pkg/queue"
)

// entry is one queued item and its terminal-once state. state is guarded by the
// owning tenant's mutex; settled is closed when state leaves stateQueued.
type entry struct {
	item    queue.Item
	state   int
	settled chan struct{}
}

const (
	stateQueued int = iota
	stateDelivered
	stateCancelled
)

// tenantQueue is the FIFO for a single tenant. items holds only still-queued
// entries in arrival order; delivered and cancelled entries are removed from it.
type tenantQueue struct {
	mu    sync.Mutex
	cond  *sync.Cond
	items []*entry
}

// cancel removes e from the queue and marks it cancelled, but only if it is
// still queued. It returns true when this call performed the transition. Because
// the check and the transition both happen under tq.mu, an entry that has
// already been delivered (or cancelled) is never cancelled again.
func (tq *tenantQueue) cancel(e *entry) bool {
	tq.mu.Lock()
	defer tq.mu.Unlock()
	if e.state != stateQueued {
		return false
	}
	for i, x := range tq.items {
		if x == e {
			tq.items = append(tq.items[:i], tq.items[i+1:]...)
			break
		}
	}
	e.state = stateCancelled
	close(e.settled)
	return true
}

// inMemoryQueue is the in-memory Queue. It maps tenantID to its tenantQueue,
// creating one on first use.
type inMemoryQueue struct {
	mu      sync.Mutex
	tenants map[string]*tenantQueue
}

// New returns a new in-memory Queue.
//
// Delivered-XOR-cancelled guarantee: an item's state lives on its entry and is
// only ever read or written while holding the owning tenant's mutex. Dequeue
// pops the front entry and sets stateDelivered under the mutex; cancellation
// (via Enqueue's ctx watcher or Ticket.Cancel) sets stateCancelled under the
// same mutex and only when the entry is still queued. The first writer wins and
// closes the entry's settled channel, so every item is either delivered exactly
// once or cancelled, never both. Cancelled entries are removed from the FIFO
// before they can be popped, so a cancelled item is never returned by Dequeue.
func New() queue.Queue {
	return &inMemoryQueue{tenants: make(map[string]*tenantQueue)}
}

// getTenant returns the tenantQueue for id, creating it on first use.
func (q *inMemoryQueue) getTenant(id string) *tenantQueue {
	q.mu.Lock()
	defer q.mu.Unlock()
	tq := q.tenants[id]
	if tq == nil {
		tq = &tenantQueue{}
		tq.cond = sync.NewCond(&tq.mu)
		q.tenants[id] = tq
	}
	return tq
}

// Enqueue appends item to the tenant FIFO and returns a Ticket. If ctx is
// already cancelled the item is not enqueued. Otherwise a watcher goroutine
// removes the item if ctx is cancelled before the item is dequeued; a removed
// item is never delivered.
func (q *inMemoryQueue) Enqueue(ctx context.Context, tenantID string, item queue.Item) (queue.Ticket, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	tq := q.getTenant(tenantID)
	e := &entry{item: item, state: stateQueued, settled: make(chan struct{})}

	tq.mu.Lock()
	tq.items = append(tq.items, e)
	tq.cond.Signal()
	tq.mu.Unlock()

	// Honour Enqueue's ctx until the item settles. Cancellation removes the
	// item; delivery ends the watcher without touching state.
	go func() {
		select {
		case <-ctx.Done():
			tq.cancel(e)
		case <-e.settled:
		}
	}()

	return &ticket{tq: tq, e: e}, nil
}

// Dequeue blocks until an item is available for the tenant or ctx is cancelled.
// It returns items in strict FIFO order.
func (q *inMemoryQueue) Dequeue(ctx context.Context, tenantID string) (queue.Item, error) {
	tq := q.getTenant(tenantID)

	tq.mu.Lock()
	defer tq.mu.Unlock()

	// Wake the cond.Wait below when ctx is cancelled. The goroutine exits when
	// Dequeue returns (stop closed) or after it has broadcast on cancellation.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			tq.mu.Lock()
			tq.cond.Broadcast()
			tq.mu.Unlock()
		case <-stop:
		}
	}()

	for {
		if len(tq.items) > 0 {
			e := tq.items[0]
			tq.items = tq.items[1:]
			if len(tq.items) == 0 {
				tq.items = nil // release the backing array
			}
			e.state = stateDelivered
			close(e.settled)
			return e.item, nil
		}
		if err := ctx.Err(); err != nil {
			return queue.Item{}, err
		}
		tq.cond.Wait()
	}
}

// Depth returns the number of currently queued items for the tenant.
func (q *inMemoryQueue) Depth(ctx context.Context, tenantID string) (int, error) {
	tq := q.getTenant(tenantID)
	tq.mu.Lock()
	defer tq.mu.Unlock()
	return len(tq.items), nil
}

// ticket is a handle to a queued entry.
type ticket struct {
	tq *tenantQueue
	e  *entry
}

// Position returns the current 0-based position among still-queued items, or
// -1 if the item has already been delivered or cancelled.
func (t *ticket) Position(ctx context.Context) (int, error) {
	t.tq.mu.Lock()
	defer t.tq.mu.Unlock()
	if t.e.state != stateQueued {
		return -1, nil
	}
	for i, x := range t.tq.items {
		if x == t.e {
			return i, nil
		}
	}
	return -1, nil
}

// Cancel removes the item from the queue if it is still queued. It is a no-op
// if the item was already delivered or cancelled.
func (t *ticket) Cancel(ctx context.Context) error {
	t.tq.cancel(t.e)
	return nil
}
