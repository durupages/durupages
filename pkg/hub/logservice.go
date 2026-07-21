// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package hub

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"time"

	"google.golang.org/grpc"

	"github.com/durupages/durupages/pkg/api"
	"github.com/durupages/durupages/pkg/usage"
)

// truncationMarker is appended to a log slice that was shortened by the
// per-request caps, so downstream consumers can tell logs were dropped.
const truncationMarker = "durupages: logs truncated"

// logServer adapts a Hub to the generated LogService server interface.
type logServer struct {
	api.UnimplementedLogServiceServer
	h *Hub
}

// RegisterLogService registers the hub's LogService/Ingest implementation on s.
func (h *Hub) RegisterLogService(s *grpc.Server) {
	api.RegisterLogServiceServer(s, &logServer{h: h})
}

// Ingest consumes batches of usage events, forwards them to the sink and acks
// each batch by ID.
func (s *logServer) Ingest(stream api.LogService_IngestServer) error {
	ctx := stream.Context()
	for {
		batch, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		s.h.processBatch(ctx, batch)
		// Ack tradeoff: we ack after best-effort delivery to the sink, even if
		// the sink returned an error. Delivery is at-least-once because the
		// shim/router retries un-acked batches; wedging the stream on a sink
		// failure would instead stall every producer behind a transient sink
		// outage. A sink error therefore drops at most the events already
		// buffered here, which is preferable to head-of-line blocking.
		if err := stream.Send(&api.IngestAck{BatchId: batch.GetBatchId()}); err != nil {
			return err
		}
	}
}

// processBatch decodes, caps and forwards one batch. Malformed events are
// skipped without failing the whole batch.
func (h *Hub) processBatch(ctx context.Context, batch *api.IngestBatch) {
	reqs := make([]usage.RequestUsage, 0, len(batch.GetRequestUsageJson()))
	for _, raw := range batch.GetRequestUsageJson() {
		var ru usage.RequestUsage
		if err := json.Unmarshal(raw, &ru); err != nil {
			log.Printf("hub: skipping malformed request_usage event: %v", err)
			continue
		}
		ru.Logs = h.capLogs(ru.Logs)
		reqs = append(reqs, ru)
	}
	statics := make([]usage.StaticAccess, 0, len(batch.GetStaticAccessJson()))
	for _, raw := range batch.GetStaticAccessJson() {
		var sa usage.StaticAccess
		if err := json.Unmarshal(raw, &sa); err != nil {
			log.Printf("hub: skipping malformed static_access event: %v", err)
			continue
		}
		statics = append(statics, sa)
	}

	if h.sink == nil {
		return
	}
	if len(reqs) > 0 {
		if err := h.sink.WriteRequestUsage(ctx, reqs); err != nil {
			log.Printf("hub: sink WriteRequestUsage: %v", err)
		}
	}
	if len(statics) > 0 {
		if err := h.sink.WriteStaticAccess(ctx, statics); err != nil {
			log.Printf("hub: sink WriteStaticAccess: %v", err)
		}
	}
}

// capLogs enforces the per-request log count and byte caps, appending a warn
// marker when it drops entries. It never mutates the input slice's backing
// array beyond reslicing plus the appended marker.
func (h *Hub) capLogs(logs []usage.LogEntry) []usage.LogEntry {
	if len(logs) == 0 {
		return logs
	}
	truncated := false
	if len(logs) > h.maxLogsPerRequest {
		logs = logs[:h.maxLogsPerRequest]
		truncated = true
	}
	total := 0
	for i := range logs {
		total += len(logs[i].Message)
		if total > h.maxLogBytesPerRequest {
			logs = logs[:i]
			truncated = true
			break
		}
	}
	if truncated {
		logs = append(logs, usage.LogEntry{
			Timestamp: time.Now(),
			Level:     "warn",
			Message:   truncationMarker,
		})
	}
	return logs
}
