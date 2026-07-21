// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

// Package queuetest provides a reusable conformance suite for queue.Queue
// implementations. Any implementation (in-memory, Redis, ...) can be verified
// against the documented contract by calling RunConformance with a factory.
package queuetest

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/durupages/durupages/pkg/queue"
)

// RunConformance runs the full queue.Queue conformance suite against the
// implementation produced by factory. factory must return a fresh, empty Queue
// on every call. Run the suite with -race to exercise the concurrency
// guarantees.
func RunConformance(t *testing.T, factory func(t *testing.T) queue.Queue) {
	t.Helper()

	t.Run("FIFOOrder", func(t *testing.T) { testFIFOOrder(t, factory) })
	t.Run("CtxCancelRemoval", func(t *testing.T) { testCtxCancelRemoval(t, factory) })
	t.Run("BlockingDequeueWakeup", func(t *testing.T) { testBlockingDequeueWakeup(t, factory) })
	t.Run("DequeueCtxCancel", func(t *testing.T) { testDequeueCtxCancel(t, factory) })
	t.Run("Depth", func(t *testing.T) { testDepth(t, factory) })
	t.Run("TicketPosition", func(t *testing.T) { testTicketPosition(t, factory) })
	t.Run("TicketCancel", func(t *testing.T) { testTicketCancel(t, factory) })
	t.Run("MultiTenantIndependence", func(t *testing.T) { testMultiTenantIndependence(t, factory) })
	t.Run("ConcurrentProducersConsumers", func(t *testing.T) { testConcurrentProducersConsumers(t, factory) })
	t.Run("DeliveredXorCancelled", func(t *testing.T) { testDeliveredXorCancelled(t, factory) })
}

func mustEnqueue(t *testing.T, q queue.Queue, ctx context.Context, tenant string, item queue.Item) queue.Ticket {
	t.Helper()
	tk, err := q.Enqueue(ctx, tenant, item)
	if err != nil {
		t.Fatalf("Enqueue(%q): unexpected error: %v", item.ID, err)
	}
	return tk
}

func mustDepth(t *testing.T, q queue.Queue, tenant string) int {
	t.Helper()
	d, err := q.Depth(context.Background(), tenant)
	if err != nil {
		t.Fatalf("Depth(%q): unexpected error: %v", tenant, err)
	}
	return d
}

func testFIFOOrder(t *testing.T, factory func(t *testing.T) queue.Queue) {
	q := factory(t)
	ctx := context.Background()
	const tenant = "t1"
	const n = 20

	for i := 0; i < n; i++ {
		mustEnqueue(t, q, ctx, tenant, queue.Item{ID: fmt.Sprintf("item-%d", i)})
	}
	for i := 0; i < n; i++ {
		got, err := q.Dequeue(ctx, tenant)
		if err != nil {
			t.Fatalf("Dequeue #%d: unexpected error: %v", i, err)
		}
		want := fmt.Sprintf("item-%d", i)
		if got.ID != want {
			t.Fatalf("Dequeue #%d: got %q, want %q (FIFO violated)", i, got.ID, want)
		}
	}
}

func testCtxCancelRemoval(t *testing.T, factory func(t *testing.T) queue.Queue) {
	q := factory(t)
	const tenant = "t1"

	// Middle item is enqueued under a cancellable ctx; cancelling it must
	// remove it so Dequeue never returns it.
	mustEnqueue(t, q, context.Background(), tenant, queue.Item{ID: "a"})
	cctx, cancel := context.WithCancel(context.Background())
	mustEnqueue(t, q, cctx, tenant, queue.Item{ID: "b"})
	mustEnqueue(t, q, context.Background(), tenant, queue.Item{ID: "c"})

	cancel()
	// Give the implementation a moment to process the cancellation.
	waitForDepth(t, q, tenant, 2)

	var got []string
	for i := 0; i < 2; i++ {
		item, err := q.Dequeue(context.Background(), tenant)
		if err != nil {
			t.Fatalf("Dequeue #%d: unexpected error: %v", i, err)
		}
		got = append(got, item.ID)
	}
	if got[0] != "a" || got[1] != "c" {
		t.Fatalf("cancelled item leaked or order wrong: got %v, want [a c]", got)
	}
	if d := mustDepth(t, q, tenant); d != 0 {
		t.Fatalf("Depth after draining: got %d, want 0", d)
	}
}

func testBlockingDequeueWakeup(t *testing.T, factory func(t *testing.T) queue.Queue) {
	q := factory(t)
	const tenant = "t1"

	type result struct {
		item queue.Item
		err  error
	}
	done := make(chan result, 1)
	go func() {
		item, err := q.Dequeue(context.Background(), tenant)
		done <- result{item, err}
	}()

	// The consumer should be blocked; nothing should arrive yet.
	select {
	case r := <-done:
		t.Fatalf("Dequeue returned before any Enqueue: %+v", r)
	case <-time.After(50 * time.Millisecond):
	}

	mustEnqueue(t, q, context.Background(), tenant, queue.Item{ID: "x"})

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("Dequeue after Enqueue: unexpected error: %v", r.err)
		}
		if r.item.ID != "x" {
			t.Fatalf("Dequeue after Enqueue: got %q, want %q", r.item.ID, "x")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Dequeue did not wake up after Enqueue")
	}
}

func testDequeueCtxCancel(t *testing.T, factory func(t *testing.T) queue.Queue) {
	q := factory(t)
	const tenant = "t1"

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := q.Dequeue(ctx, tenant)
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("Dequeue returned before ctx cancel: err=%v", err)
	case <-time.After(50 * time.Millisecond):
	}

	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Dequeue after ctx cancel: want error, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Dequeue did not return after ctx cancel")
	}
}

func testDepth(t *testing.T, factory func(t *testing.T) queue.Queue) {
	q := factory(t)
	ctx := context.Background()
	const tenant = "t1"

	if d := mustDepth(t, q, tenant); d != 0 {
		t.Fatalf("initial Depth: got %d, want 0", d)
	}
	for i := 1; i <= 5; i++ {
		mustEnqueue(t, q, ctx, tenant, queue.Item{ID: fmt.Sprintf("i%d", i)})
		if d := mustDepth(t, q, tenant); d != i {
			t.Fatalf("Depth after %d enqueues: got %d, want %d", i, d, i)
		}
	}
	for i := 4; i >= 0; i-- {
		if _, err := q.Dequeue(ctx, tenant); err != nil {
			t.Fatalf("Dequeue: unexpected error: %v", err)
		}
		if d := mustDepth(t, q, tenant); d != i {
			t.Fatalf("Depth after dequeue: got %d, want %d", d, i)
		}
	}
}

func testTicketPosition(t *testing.T, factory func(t *testing.T) queue.Queue) {
	q := factory(t)
	ctx := context.Background()
	const tenant = "t1"

	var tickets []queue.Ticket
	for i := 0; i < 4; i++ {
		tickets = append(tickets, mustEnqueue(t, q, ctx, tenant, queue.Item{ID: fmt.Sprintf("i%d", i)}))
	}
	for i, tk := range tickets {
		pos, err := tk.Position(ctx)
		if err != nil {
			t.Fatalf("Position #%d: unexpected error: %v", i, err)
		}
		if pos != i {
			t.Fatalf("Position #%d: got %d, want %d", i, pos, i)
		}
	}

	// Dequeue the head; remaining tickets shift forward by one.
	if _, err := q.Dequeue(ctx, tenant); err != nil {
		t.Fatalf("Dequeue: unexpected error: %v", err)
	}
	for i, tk := range tickets[1:] {
		pos, err := tk.Position(ctx)
		if err != nil {
			t.Fatalf("Position after dequeue #%d: unexpected error: %v", i, err)
		}
		if pos != i {
			t.Fatalf("Position after dequeue #%d: got %d, want %d", i, pos, i)
		}
	}
}

func testTicketCancel(t *testing.T, factory func(t *testing.T) queue.Queue) {
	q := factory(t)
	ctx := context.Background()
	const tenant = "t1"

	mustEnqueue(t, q, ctx, tenant, queue.Item{ID: "a"})
	tkB := mustEnqueue(t, q, ctx, tenant, queue.Item{ID: "b"})
	mustEnqueue(t, q, ctx, tenant, queue.Item{ID: "c"})

	if err := tkB.Cancel(ctx); err != nil {
		t.Fatalf("Cancel: unexpected error: %v", err)
	}
	if d := mustDepth(t, q, tenant); d != 2 {
		t.Fatalf("Depth after cancel: got %d, want 2", d)
	}

	// Cancel is idempotent.
	if err := tkB.Cancel(ctx); err != nil {
		t.Fatalf("second Cancel: unexpected error: %v", err)
	}

	var got []string
	for i := 0; i < 2; i++ {
		item, err := q.Dequeue(ctx, tenant)
		if err != nil {
			t.Fatalf("Dequeue #%d: unexpected error: %v", i, err)
		}
		got = append(got, item.ID)
	}
	if got[0] != "a" || got[1] != "c" {
		t.Fatalf("cancelled item leaked: got %v, want [a c]", got)
	}
}

func testMultiTenantIndependence(t *testing.T, factory func(t *testing.T) queue.Queue) {
	q := factory(t)
	ctx := context.Background()

	mustEnqueue(t, q, ctx, "t1", queue.Item{ID: "t1-a"})
	mustEnqueue(t, q, ctx, "t1", queue.Item{ID: "t1-b"})
	mustEnqueue(t, q, ctx, "t2", queue.Item{ID: "t2-a"})

	if d := mustDepth(t, q, "t1"); d != 2 {
		t.Fatalf("Depth(t1): got %d, want 2", d)
	}
	if d := mustDepth(t, q, "t2"); d != 1 {
		t.Fatalf("Depth(t2): got %d, want 1", d)
	}

	item, err := q.Dequeue(ctx, "t2")
	if err != nil {
		t.Fatalf("Dequeue(t2): unexpected error: %v", err)
	}
	if item.ID != "t2-a" {
		t.Fatalf("Dequeue(t2): got %q, want %q", item.ID, "t2-a")
	}
	// t1 is untouched.
	if d := mustDepth(t, q, "t1"); d != 2 {
		t.Fatalf("Depth(t1) after t2 dequeue: got %d, want 2", d)
	}
}

func testConcurrentProducersConsumers(t *testing.T, factory func(t *testing.T) queue.Queue) {
	q := factory(t)
	const (
		tenant    = "t1"
		producers = 8
		perProd   = 200
		total     = producers * perProd
	)

	var wg sync.WaitGroup
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := 0; i < perProd; i++ {
				mustEnqueue(t, q, context.Background(), tenant, queue.Item{ID: fmt.Sprintf("p%d-i%d", p, i)})
			}
		}(p)
	}

	var seen sync.Map
	var dups int64
	var consumed int64
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var cwg sync.WaitGroup
	for c := 0; c < 4; c++ {
		cwg.Add(1)
		go func() {
			defer cwg.Done()
			for {
				item, err := q.Dequeue(ctx, tenant)
				if err != nil {
					return
				}
				if _, loaded := seen.LoadOrStore(item.ID, struct{}{}); loaded {
					atomic.AddInt64(&dups, 1)
				}
				if atomic.AddInt64(&consumed, 1) == total {
					cancel()
				}
			}
		}()
	}

	wg.Wait()
	cwg.Wait()

	if got := atomic.LoadInt64(&consumed); got != total {
		t.Fatalf("consumed: got %d, want %d", got, total)
	}
	if d := atomic.LoadInt64(&dups); d != 0 {
		t.Fatalf("duplicate deliveries: got %d, want 0", d)
	}
	if d := mustDepth(t, q, tenant); d != 0 {
		t.Fatalf("final Depth: got %d, want 0", d)
	}
}

// testDeliveredXorCancelled races cancellations against deliveries under heavy
// concurrency. Using only the public interface (Cancel does not report whether
// it won the race), it asserts the observable atomicity invariants:
//
//   - No item is ever delivered more than once (delivery is exactly-once).
//   - Items enqueued under a ctx that is never cancelled are ALWAYS delivered
//     (a cancellation racing a neighbour must not drop a non-cancelled item).
//   - The queue drains fully (final Depth == 0), so no item is both delivered
//     and left occupying the queue.
//
// A non-atomic implementation that lets an item be both delivered and cancelled
// shows up here as a duplicate delivery, a lost stable item, or a non-zero
// residual depth.
func testDeliveredXorCancelled(t *testing.T, factory func(t *testing.T) queue.Queue) {
	q := factory(t)
	const (
		tenant = "t1"
		n      = 3000
	)

	var delivered sync.Map // id -> delivery count
	var dupes int64

	consumeCtx, stopConsumers := context.WithCancel(context.Background())
	var cwg sync.WaitGroup
	for c := 0; c < 4; c++ {
		cwg.Add(1)
		go func() {
			defer cwg.Done()
			for {
				item, err := q.Dequeue(consumeCtx, tenant)
				if err != nil {
					return
				}
				prev, _ := delivered.LoadOrStore(item.ID, new(int64))
				if atomic.AddInt64(prev.(*int64), 1) > 1 {
					atomic.AddInt64(&dupes, 1)
				}
			}
		}()
	}

	// Producers enqueue concurrently. Even-indexed items race a cancel against
	// delivery; odd-indexed items are stable (never cancelled) and must all be
	// delivered.
	var pwg sync.WaitGroup
	for i := 0; i < n; i++ {
		pwg.Add(1)
		go func(i int) {
			defer pwg.Done()
			id := fmt.Sprintf("item-%d", i)
			if i%2 == 0 {
				ctx, cancel := context.WithCancel(context.Background())
				if _, err := q.Enqueue(ctx, tenant, queue.Item{ID: id}); err != nil {
					cancel()
					return
				}
				cancel() // races delivery
			} else {
				if _, err := q.Enqueue(context.Background(), tenant, queue.Item{ID: id}); err != nil {
					t.Errorf("stable Enqueue(%q): %v", id, err)
				}
			}
		}(i)
	}
	pwg.Wait()

	// The queue must drain: every item is either delivered or cancel-removed.
	waitForDepth(t, q, tenant, 0)
	// Let any last cancellation settle, then confirm it stays drained.
	time.Sleep(20 * time.Millisecond)
	waitForDepth(t, q, tenant, 0)
	stopConsumers()
	cwg.Wait()

	if d := atomic.LoadInt64(&dupes); d != 0 {
		t.Fatalf("duplicate deliveries: got %d, want 0 (delivery not exactly-once)", d)
	}
	// Every stable (odd) item must have been delivered exactly once.
	for i := 1; i < n; i += 2 {
		id := fmt.Sprintf("item-%d", i)
		v, ok := delivered.Load(id)
		if !ok {
			t.Fatalf("stable item %q was never delivered (lost by a racing cancel)", id)
		}
		if got := atomic.LoadInt64(v.(*int64)); got != 1 {
			t.Fatalf("stable item %q delivered %d times, want 1", id, got)
		}
	}
	// Every even item was delivered at most once (never twice).
	for i := 0; i < n; i += 2 {
		id := fmt.Sprintf("item-%d", i)
		if v, ok := delivered.Load(id); ok {
			if got := atomic.LoadInt64(v.(*int64)); got != 1 {
				t.Fatalf("racing item %q delivered %d times, want 0 or 1", id, got)
			}
		}
	}
	if d := mustDepth(t, q, tenant); d != 0 {
		t.Fatalf("final Depth: got %d, want 0", d)
	}
}

// waitForDepth polls Depth until it reaches want or the deadline elapses.
func waitForDepth(t *testing.T, q queue.Queue, tenant string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		d := mustDepth(t, q, tenant)
		if d == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Depth(%q) did not reach %d (last=%d)", tenant, want, d)
		}
		time.Sleep(time.Millisecond)
	}
}
