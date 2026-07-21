// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package shim

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/durupages/durupages/pkg/api"
	"github.com/durupages/durupages/pkg/runtime"
	"github.com/durupages/durupages/pkg/usage"
)

// maxTailBody caps the tail collector request body.
const maxTailBody = 1 << 20 // 1 MiB

// traceRecord mirrors the JSON the generated tail worker POSTs (see
// workerdruntime tailWorkerJS). Timestamps and cpu/wall times are milliseconds.
type traceRecord struct {
	ScriptName string  `json:"scriptName"`
	Outcome    string  `json:"outcome"`
	CPUTime    float64 `json:"cpuTime"`
	WallTime   float64 `json:"wallTime"`
	Logs       []struct {
		Timestamp float64 `json:"timestamp"`
		Level     string  `json:"level"`
		Message   string  `json:"message"`
	} `json:"logs"`
	Exceptions []struct {
		Timestamp float64 `json:"timestamp"`
		Name      string  `json:"name"`
		Message   string  `json:"message"`
		Stack     string  `json:"stack"`
	} `json:"exceptions"`
	Event *struct {
		Request struct {
			URL     string            `json:"url"`
			Method  string            `json:"method"`
			Headers map[string]string `json:"headers"`
		} `json:"request"`
		Response *struct {
			Status int `json:"status"`
		} `json:"response"`
	} `json:"event"`
}

// serveCollector handles the tail worker's POST of a batch of trace records. It
// keys each record by the X-DuruPages-Request-Id header injected on the request
// and hands the derived RequestTrace to the correlator for the proxy to pick up.
func (s *Shim) serveCollector(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxTailBody+1))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	if len(body) > maxTailBody {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	var records []traceRecord
	if err := json.Unmarshal(body, &records); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	for i := range records {
		if trace, ok := records[i].toTrace(); ok {
			s.cor.deliver(trace)
		}
	}
	w.WriteHeader(http.StatusOK)
}

// toTrace converts a tail record into a runtime.RequestTrace, extracting the
// correlation request ID from the event request headers. ok is false when no
// request ID is present (the trace cannot be correlated).
func (rec *traceRecord) toTrace() (runtime.RequestTrace, bool) {
	if rec.Event == nil {
		return runtime.RequestTrace{}, false
	}
	var requestID string
	for name, val := range rec.Event.Request.Headers {
		if strings.EqualFold(name, api.HeaderRequestID) {
			requestID = val
			break
		}
	}
	if requestID == "" {
		return runtime.RequestTrace{}, false
	}
	trace := runtime.RequestTrace{
		RequestID: requestID,
		PageID:    rec.ScriptName,
		Outcome:   rec.Outcome,
		CPUTime:   msToDuration(rec.CPUTime),
		WallTime:  msToDuration(rec.WallTime),
	}
	for _, l := range rec.Logs {
		trace.Logs = append(trace.Logs, usage.LogEntry{
			Timestamp: msToTime(l.Timestamp),
			Level:     l.Level,
			Message:   l.Message,
		})
	}
	for _, e := range rec.Exceptions {
		trace.Exceptions = append(trace.Exceptions, usage.Exception{
			Timestamp: msToTime(e.Timestamp),
			Name:      e.Name,
			Message:   e.Message,
			Stack:     e.Stack,
		})
	}
	return trace, true
}

func msToDuration(ms float64) time.Duration {
	return time.Duration(ms * float64(time.Millisecond))
}

func msToTime(ms float64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	sec := int64(ms) / 1000
	nsec := (int64(ms) % 1000) * int64(time.Millisecond)
	return time.Unix(sec, nsec).UTC()
}

// correlator holds per-request traces keyed by request ID, letting the proxy
// wait briefly for the tail trace of the request it just served.
type correlator struct {
	now func() time.Time
	mu  sync.Mutex
	m   map[string]*corEntry
}

type corEntry struct {
	trace   *runtime.RequestTrace
	ch      chan struct{} // closed when trace arrives
	expires time.Time
}

// corTTL is how long an unclaimed trace is retained.
const corTTL = 5 * time.Second

func newCorrelator(now func() time.Time) *correlator {
	return &correlator{now: now, m: map[string]*corEntry{}}
}

// expect registers interest in requestID before the request is forwarded.
func (c *correlator) expect(requestID string) {
	if requestID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.m[requestID]; !ok {
		c.m[requestID] = &corEntry{ch: make(chan struct{}), expires: c.now().Add(corTTL)}
	}
}

// deliver stores a trace and wakes any waiter.
func (c *correlator) deliver(trace runtime.RequestTrace) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[trace.RequestID]
	if !ok {
		e = &corEntry{ch: make(chan struct{}), expires: c.now().Add(corTTL)}
		c.m[trace.RequestID] = e
	}
	if e.trace == nil {
		t := trace
		e.trace = &t
		close(e.ch)
	}
}

// wait blocks up to timeout for requestID's trace, then forgets the entry.
func (c *correlator) wait(ctx context.Context, requestID string, timeout time.Duration) *runtime.RequestTrace {
	if requestID == "" {
		return nil
	}
	c.mu.Lock()
	e := c.m[requestID]
	c.mu.Unlock()
	if e == nil {
		return nil
	}

	select {
	case <-e.ch:
	case <-time.After(timeout):
	case <-ctx.Done():
	}

	c.mu.Lock()
	trace := e.trace
	delete(c.m, requestID)
	c.mu.Unlock()
	return trace
}

// janitor periodically drops expired, unclaimed entries.
func (c *correlator) janitor(ctx context.Context) {
	ticker := time.NewTicker(corTTL)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := c.now()
			c.mu.Lock()
			for id, e := range c.m {
				if now.After(e.expires) {
					delete(c.m, id)
				}
			}
			c.mu.Unlock()
		}
	}
}
