// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package shim

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/durupages/durupages/pkg/api"
	"github.com/durupages/durupages/pkg/usage"
)

// usageLine is the pod-log JSON Lines record: the usage event inlined under a
// "type" discriminator, matching hub/stdoutsink so both paths share one schema.
type usageLine struct {
	Type string `json:"type"`
	usage.RequestUsage
}

// emitter delivers RequestUsage events either to the hub over gRPC (LogClient
// set) or as JSON lines to a writer (pod-log mode). Delivery never blocks the
// serving path: the gRPC path buffers with bounded capacity and drops on
// overflow, counting the drops.
type emitter struct {
	client api.LogServiceClient
	w      io.Writer
	mu     sync.Mutex // serializes pod-log writes

	ch      chan []byte
	dropped atomic.Int64
}

const emitBufferSize = 4096

func newEmitter(client api.LogServiceClient, w io.Writer) *emitter {
	if w == nil {
		w = os.Stdout
	}
	return &emitter{
		client: client,
		w:      w,
		ch:     make(chan []byte, emitBufferSize),
	}
}

// emit records one usage event.
func (e *emitter) emit(u usage.RequestUsage) {
	line, err := json.Marshal(usageLine{Type: "request_usage", RequestUsage: u})
	if err != nil {
		return
	}
	if e.client == nil {
		e.mu.Lock()
		_, _ = e.w.Write(append(line, '\n'))
		e.mu.Unlock()
		return
	}
	select {
	case e.ch <- line:
	default:
		e.dropped.Add(1)
	}
}

// run drives the gRPC batching loop; in pod-log mode it is a no-op until ctx
// ends.
func (e *emitter) run(ctx context.Context) {
	if e.client == nil {
		<-ctx.Done()
		return
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var pending [][]byte
	flush := func() {
		if len(pending) == 0 {
			return
		}
		e.flush(ctx, pending)
		pending = pending[:0]
	}
	for {
		select {
		case <-ctx.Done():
			flush()
			return
		case line := <-e.ch:
			pending = append(pending, line)
			if len(pending) >= 1000 {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

var batchSeq atomic.Int64

// flush sends one batch to the hub via a fresh Ingest stream, attaching any
// accumulated drop count to the first event.
func (e *emitter) flush(ctx context.Context, lines [][]byte) {
	stream, err := e.client.Ingest(ctx)
	if err != nil {
		return
	}
	batch := &api.IngestBatch{
		BatchId:          batchSeq.Add(1),
		RequestUsageJson: lines,
	}
	if dropped := e.dropped.Swap(0); dropped > 0 && len(lines) > 0 {
		if patched, ok := patchDropped(lines[0], dropped); ok {
			batch.RequestUsageJson[0] = patched
		}
	}
	if err := stream.Send(batch); err != nil {
		return
	}
	_, _ = stream.Recv()
	_ = stream.CloseSend()
}

// patchDropped re-marshals the first event with the drop count set.
func patchDropped(line []byte, dropped int64) ([]byte, bool) {
	var u usageLine
	if json.Unmarshal(line, &u) != nil {
		return nil, false
	}
	u.DroppedEvents = dropped
	out, err := json.Marshal(u)
	if err != nil {
		return nil, false
	}
	return out, true
}
