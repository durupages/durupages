// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package router

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/durupages/durupages/pkg/api"
	"github.com/durupages/durupages/pkg/usage"
)

// Batching parameters for LogService mode (docs/ARCHITECTURE.md 9.3): flush on
// a 1s tick or 1,000 events, with a bounded 10k buffer that drops the oldest
// events on overflow so the serving path never blocks.
const (
	flushInterval     = time.Second
	flushBatchSize    = 1000
	maxBufferedEvents = 10000
)

// This file implements the USAGE log: the per-request StaticAccess records the
// hub aggregates for billing and for the customer-visible log stream, shipped
// over Options.LogClient or written as pod-log JSON lines to Options.LogWriter.
//
// It is NOT the router's operational log. Diagnostics about the router itself
// (why a request produced a 502, access lines) go to Options.Logger and live in
// oplog.go. Keep the two apart: this stream's consumers parse it as one
// StaticAccess JSON object per line.

// logStatic builds and emits a StaticAccess event for one static response.
// Sensitive request headers (Authorization/Cookie/...) are omitted: only a
// small allowlist (host, user-agent, referer) is recorded.
func (rt *Router) logStatic(r *http.Request, host string, page *api.PageInfo, status int, bytesSent int64) {
	rt.usageLog.emit(usage.StaticAccess{
		TenantID:     page.GetTenantId(),
		PageID:       page.GetPageId(),
		DeploymentID: page.GetActiveDeploymentId(),
		Timestamp:    rt.now(),
		BytesSent:    bytesSent,
		Event: usage.Event{
			Request: usage.RequestInfo{
				URL:     requestURL(r, host),
				Method:  r.Method,
				Headers: allowlistHeaders(r, host),
			},
			Response: usage.ResponseInfo{Status: status},
		},
	})
}

func requestURL(r *http.Request, host string) string {
	u := *r.URL
	if u.Host == "" {
		u.Host = host
	}
	if u.Scheme == "" {
		u.Scheme = "https"
	}
	return u.String()
}

func allowlistHeaders(r *http.Request, host string) map[string]string {
	h := map[string]string{"host": host}
	if v := r.Header.Get("User-Agent"); v != "" {
		h["user-agent"] = v
	}
	if v := r.Header.Get("Referer"); v != "" {
		h["referer"] = v
	}
	return h
}

// usageLogger emits StaticAccess events either to a LogService.Ingest stream
// (batched) or, in pod-log mode, as JSON lines to a writer.
type usageLogger struct {
	now func() time.Time

	// pod-log mode.
	mu sync.Mutex
	w  io.Writer

	// LogService mode (nil in pod-log mode).
	g *grpcLogger
}

func newUsageLogger(client api.LogServiceClient, w io.Writer, now func() time.Time) *usageLogger {
	l := &usageLogger{now: now, w: w}
	if client != nil {
		l.g = newGRPCLogger(client)
	}
	return l
}

// podLogLine is the pod-log JSON shape: a StaticAccess with a discriminator.
type podLogLine struct {
	Type string `json:"type"`
	usage.StaticAccess
}

func (l *usageLogger) emit(ev usage.StaticAccess) {
	if l.g != nil {
		l.g.emit(ev)
		return
	}
	b, err := json.Marshal(podLogLine{Type: "static_access", StaticAccess: ev})
	if err != nil {
		return
	}
	b = append(b, '\n')
	l.mu.Lock()
	_, _ = l.w.Write(b)
	l.mu.Unlock()
}

func (l *usageLogger) close() {
	if l.g != nil {
		l.g.close()
	}
}

// grpcLogger batches StaticAccess events to a LogService.Ingest stream. Sends
// are asynchronous and never block callers; on hub trouble events accumulate in
// a bounded buffer and the oldest are dropped past the cap.
type grpcLogger struct {
	client api.LogServiceClient

	mu      sync.Mutex
	buf     []usage.StaticAccess
	dropped int64
	batchID int64

	wake chan struct{}
	stop chan struct{}
	done chan struct{}
	once sync.Once

	// stream state, only touched by run().
	stream  api.LogService_IngestClient
	scancel context.CancelFunc
}

func newGRPCLogger(client api.LogServiceClient) *grpcLogger {
	g := &grpcLogger{
		client: client,
		wake:   make(chan struct{}, 1),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	go g.run()
	return g
}

func (g *grpcLogger) emit(ev usage.StaticAccess) {
	g.mu.Lock()
	if len(g.buf) >= maxBufferedEvents {
		g.buf = g.buf[1:] // drop oldest
		g.dropped++
	}
	g.buf = append(g.buf, ev)
	n := len(g.buf)
	g.mu.Unlock()
	if n >= flushBatchSize {
		select {
		case g.wake <- struct{}{}:
		default:
		}
	}
}

func (g *grpcLogger) run() {
	defer close(g.done)
	t := time.NewTicker(flushInterval)
	defer t.Stop()
	for {
		select {
		case <-g.stop:
			g.flush()
			return
		case <-t.C:
			g.flush()
		case <-g.wake:
			g.flush()
		}
	}
}

func (g *grpcLogger) flush() {
	g.mu.Lock()
	if len(g.buf) == 0 {
		g.mu.Unlock()
		return
	}
	batch := g.buf
	g.buf = nil
	g.batchID++
	id := g.batchID
	g.mu.Unlock()

	jsons := make([][]byte, 0, len(batch))
	for i := range batch {
		if b, err := json.Marshal(batch[i]); err == nil {
			jsons = append(jsons, b)
		}
	}
	if err := g.ensureStream(); err != nil {
		return // drop this batch; a later flush retries with a fresh stream
	}
	if err := g.stream.Send(&api.IngestBatch{BatchId: id, StaticAccessJson: jsons}); err != nil {
		g.resetStream()
	}
}

func (g *grpcLogger) ensureStream() error {
	if g.stream != nil {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	s, err := g.client.Ingest(ctx)
	if err != nil {
		cancel()
		return err
	}
	g.stream = s
	g.scancel = cancel
	// Drain acks so the stream's flow control never stalls.
	go func() {
		for {
			if _, err := s.Recv(); err != nil {
				return
			}
		}
	}()
	return nil
}

func (g *grpcLogger) resetStream() {
	if g.scancel != nil {
		g.scancel()
		g.scancel = nil
	}
	g.stream = nil
}

func (g *grpcLogger) close() {
	g.once.Do(func() { close(g.stop) })
	<-g.done
	g.resetStream()
}
