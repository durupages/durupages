// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

// Package stdoutsink is the default hub.LogSink: it writes one JSON object per
// event to an io.Writer (os.Stdout by default), as newline-delimited JSON. Each
// line wraps the pkg/usage event with a "type" discriminator
// ("request_usage" | "static_access") so a single stream can carry both.
package stdoutsink

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"sync"

	"github.com/durupages/durupages/pkg/hub"
	"github.com/durupages/durupages/pkg/usage"
)

// compile-time check that *Sink implements hub.LogSink.
var _ hub.LogSink = (*Sink)(nil)

// Sink writes events as JSON lines. It is safe for concurrent use; writes are
// serialized so lines never interleave.
type Sink struct {
	mu  sync.Mutex
	enc *json.Encoder
}

// New returns a Sink writing to w. A nil w defaults to os.Stdout.
func New(w io.Writer) *Sink {
	if w == nil {
		w = os.Stdout
	}
	return &Sink{enc: json.NewEncoder(w)}
}

// requestUsageLine and staticAccessLine are the wrapped wire records. The
// event is inlined at the top level alongside the type discriminator.
type requestUsageLine struct {
	Type string `json:"type"`
	usage.RequestUsage
}

type staticAccessLine struct {
	Type string `json:"type"`
	usage.StaticAccess
}

// WriteRequestUsage writes one JSON line per request-usage event.
func (s *Sink) WriteRequestUsage(ctx context.Context, events []usage.RequestUsage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range events {
		if err := s.enc.Encode(requestUsageLine{Type: "request_usage", RequestUsage: events[i]}); err != nil {
			return err
		}
	}
	return nil
}

// WriteStaticAccess writes one JSON line per static-access event.
func (s *Sink) WriteStaticAccess(ctx context.Context, events []usage.StaticAccess) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range events {
		if err := s.enc.Encode(staticAccessLine{Type: "static_access", StaticAccess: events[i]}); err != nil {
			return err
		}
	}
	return nil
}
