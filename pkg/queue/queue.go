// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

// Package queue defines the Queue extension point: per-tenant FIFO waiting
// for worker slots. The default implementation is in-memory (package
// inmemory); e.g. a Redis-backed implementation can replace it.
//
// The queue is only responsible for ordering. Slot/lease management, timeout
// decisions and autoscaling are performed by the controller dispatcher.
//
// Implementation contract (verified by the conformance suite):
//   - FIFO per tenant.
//   - Enqueue must respect ctx: when ctx is cancelled (caller gone or queue
//     timeout), the item must be removed and never returned by Dequeue.
//   - Dequeue blocks until an item is available or ctx is cancelled.
package queue

import (
	"context"
	"time"
)

// Item is one queued request.
type Item struct {
	ID         string
	PageID     string
	EnqueuedAt time.Time
	// Deadline is when the queue wait times out (page QueueTimeout applied by
	// the dispatcher). Implementations may drop items past their deadline.
	Deadline time.Time
	Meta     map[string]string
}

// Ticket is a handle for a queued item.
type Ticket interface {
	// Position returns the current 0-based position (best effort).
	Position(ctx context.Context) (int, error)
	// Cancel removes the item from the queue if still queued.
	Cancel(ctx context.Context) error
}

// Queue is a per-tenant FIFO.
type Queue interface {
	Enqueue(ctx context.Context, tenantID string, item Item) (Ticket, error)
	Dequeue(ctx context.Context, tenantID string) (Item, error)
	Depth(ctx context.Context, tenantID string) (int, error)
}
