// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package hub

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/durupages/durupages/pkg/api"
	"github.com/durupages/durupages/pkg/storage"
	"github.com/durupages/durupages/pkg/usage"
)

// recordingSink captures events forwarded by the hub and can be told to fail.
type recordingSink struct {
	mu       sync.Mutex
	requests []usage.RequestUsage
	statics  []usage.StaticAccess
	failReq  bool
}

func (s *recordingSink) WriteRequestUsage(ctx context.Context, events []usage.RequestUsage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failReq {
		return errors.New("sink boom")
	}
	s.requests = append(s.requests, events...)
	return nil
}

func (s *recordingSink) WriteStaticAccess(ctx context.Context, events []usage.StaticAccess) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statics = append(s.statics, events...)
	return nil
}

func (s *recordingSink) snapshot() ([]usage.RequestUsage, []usage.StaticAccess) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]usage.RequestUsage(nil), s.requests...), append([]usage.StaticAccess(nil), s.statics...)
}

// fakeStorage is a stand-in Storage for LogService tests (the bundle API is
// unexercised here). Every Get misses.
type fakeStorage struct{}

func (fakeStorage) Get(context.Context, string) (io.ReadCloser, storage.ObjectInfo, error) {
	return nil, storage.ObjectInfo{}, storage.ErrNotFound
}
func (fakeStorage) Put(context.Context, string, io.Reader, int64, string) error { return nil }
func (fakeStorage) Delete(context.Context, string) error                        { return nil }
func (fakeStorage) List(context.Context, string) ([]storage.ObjectInfo, error)  { return nil, nil }

// newIngestClient spins up a bufconn-backed hub LogService and returns a client
// plus the sink for assertions.
func newIngestClient(t *testing.T, opts Options) (api.LogServiceClient, *recordingSink, func()) {
	t.Helper()
	sink := &recordingSink{}
	opts.Sink = sink
	if opts.Logger == nil {
		// These tests deliberately feed malformed events and a failing sink;
		// keep their (expected) log lines out of the test output.
		opts.Logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	h, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	h.RegisterLogService(srv)
	go srv.Serve(lis)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, s string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	cleanup := func() {
		conn.Close()
		srv.Stop()
		lis.Close()
	}
	return api.NewLogServiceClient(conn), sink, cleanup
}

// zeroReader is a deterministic key source for tests.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

func optionsWithKey(t *testing.T) Options {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(zeroReader{})
	if err != nil {
		t.Fatal(err)
	}
	return Options{Storage: fakeStorage{}, JWTPublicKey: pub}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestIngestBatchAckAndDelivery(t *testing.T) {
	client, sink, cleanup := newIngestClient(t, optionsWithKey(t))
	defer cleanup()

	stream, err := client.Ingest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ru := usage.RequestUsage{RequestID: "r1", TenantID: testTenant, PageID: testPage}
	sa := usage.StaticAccess{TenantID: testTenant, PageID: testPage, BytesSent: 42}
	batch := &api.IngestBatch{
		BatchId:          7,
		RequestUsageJson: [][]byte{mustJSON(t, ru)},
		StaticAccessJson: [][]byte{mustJSON(t, sa)},
	}
	if err := stream.Send(batch); err != nil {
		t.Fatal(err)
	}
	ack, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if ack.GetBatchId() != 7 {
		t.Fatalf("ack batch_id = %d, want 7", ack.GetBatchId())
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}

	reqs, statics := sink.snapshot()
	if len(reqs) != 1 || reqs[0].RequestID != "r1" {
		t.Fatalf("request events = %+v", reqs)
	}
	if len(statics) != 1 || statics[0].BytesSent != 42 {
		t.Fatalf("static events = %+v", statics)
	}
}

func TestIngestMalformedEventSkipped(t *testing.T) {
	client, sink, cleanup := newIngestClient(t, optionsWithKey(t))
	defer cleanup()

	stream, err := client.Ingest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	good := usage.RequestUsage{RequestID: "good"}
	batch := &api.IngestBatch{
		BatchId:          1,
		RequestUsageJson: [][]byte{[]byte("{not json"), mustJSON(t, good)},
	}
	if err := stream.Send(batch); err != nil {
		t.Fatal(err)
	}
	ack, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if ack.GetBatchId() != 1 {
		t.Fatalf("ack = %d", ack.GetBatchId())
	}
	_ = stream.CloseSend()

	reqs, _ := sink.snapshot()
	if len(reqs) != 1 || reqs[0].RequestID != "good" {
		t.Fatalf("expected only the valid event, got %+v", reqs)
	}
}

func TestIngestTruncationByCount(t *testing.T) {
	opts := optionsWithKey(t)
	opts.MaxLogsPerRequest = 3
	client, sink, cleanup := newIngestClient(t, opts)
	defer cleanup()

	logs := make([]usage.LogEntry, 10)
	for i := range logs {
		logs[i] = usage.LogEntry{Level: "log", Message: "m"}
	}
	ru := usage.RequestUsage{RequestID: "r", Logs: logs}

	stream, _ := client.Ingest(context.Background())
	_ = stream.Send(&api.IngestBatch{BatchId: 1, RequestUsageJson: [][]byte{mustJSON(t, ru)}})
	if _, err := stream.Recv(); err != nil {
		t.Fatal(err)
	}
	_ = stream.CloseSend()

	reqs, _ := sink.snapshot()
	if len(reqs) != 1 {
		t.Fatalf("got %d events", len(reqs))
	}
	got := reqs[0].Logs
	// 3 kept + 1 truncation marker.
	if len(got) != 4 {
		t.Fatalf("log count = %d, want 4", len(got))
	}
	last := got[len(got)-1]
	if last.Level != "warn" || last.Message != truncationMarker {
		t.Fatalf("last log = %+v, want truncation marker", last)
	}
}

func TestIngestTruncationByBytes(t *testing.T) {
	opts := optionsWithKey(t)
	opts.MaxLogsPerRequest = 1000
	opts.MaxLogBytesPerRequest = 20
	client, sink, cleanup := newIngestClient(t, opts)
	defer cleanup()

	logs := []usage.LogEntry{
		{Level: "log", Message: "0123456789"}, // 10 bytes, total 10
		{Level: "log", Message: "0123456789"}, // 10 bytes, total 20 (== cap, kept)
		{Level: "log", Message: "over"},       // pushes over 20 -> dropped
	}
	ru := usage.RequestUsage{RequestID: "r", Logs: logs}

	stream, _ := client.Ingest(context.Background())
	_ = stream.Send(&api.IngestBatch{BatchId: 1, RequestUsageJson: [][]byte{mustJSON(t, ru)}})
	if _, err := stream.Recv(); err != nil {
		t.Fatal(err)
	}
	_ = stream.CloseSend()

	reqs, _ := sink.snapshot()
	got := reqs[0].Logs
	// 2 kept + marker.
	if len(got) != 3 {
		t.Fatalf("log count = %d, want 3: %+v", len(got), got)
	}
	if got[len(got)-1].Message != truncationMarker {
		t.Fatalf("expected truncation marker, got %+v", got[len(got)-1])
	}
}

func TestIngestNoTruncationWhenUnderCaps(t *testing.T) {
	opts := optionsWithKey(t)
	client, sink, cleanup := newIngestClient(t, opts)
	defer cleanup()

	logs := []usage.LogEntry{{Level: "log", Message: "hi"}}
	ru := usage.RequestUsage{RequestID: "r", Logs: logs}
	stream, _ := client.Ingest(context.Background())
	_ = stream.Send(&api.IngestBatch{BatchId: 1, RequestUsageJson: [][]byte{mustJSON(t, ru)}})
	if _, err := stream.Recv(); err != nil {
		t.Fatal(err)
	}
	_ = stream.CloseSend()

	reqs, _ := sink.snapshot()
	if len(reqs[0].Logs) != 1 {
		t.Fatalf("logs = %+v, want unchanged", reqs[0].Logs)
	}
}

// TestIngestSinkErrorStillAcks verifies a sink failure does not wedge the
// stream: the batch is still acked (at-least-once via producer retries).
func TestIngestSinkErrorStillAcks(t *testing.T) {
	opts := optionsWithKey(t)
	client, sink, cleanup := newIngestClient(t, opts)
	defer cleanup()
	sink.mu.Lock()
	sink.failReq = true
	sink.mu.Unlock()

	stream, _ := client.Ingest(context.Background())
	ru := usage.RequestUsage{RequestID: "r"}
	_ = stream.Send(&api.IngestBatch{BatchId: 99, RequestUsageJson: [][]byte{mustJSON(t, ru)}})
	ack, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if ack.GetBatchId() != 99 {
		t.Fatalf("ack = %d, want 99 despite sink error", ack.GetBatchId())
	}
	_ = stream.CloseSend()
}

// TestIngestFailuresAreLogged covers the two ways ingest loses data quietly: a
// producer shipping an undecodable event, and a sink rejecting a batch that was
// already acked. Both used to vanish into an unstructured log.Printf.
func TestIngestFailuresAreLogged(t *testing.T) {
	logs := &logCapture{}
	opts := optionsWithKey(t)
	opts.Logger = logs.logger()
	client, sink, cleanup := newIngestClient(t, opts)
	defer cleanup()
	sink.mu.Lock()
	sink.failReq = true
	sink.mu.Unlock()

	stream, err := client.Ingest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	good := usage.RequestUsage{RequestID: "good"}
	batch := &api.IngestBatch{
		BatchId:          42,
		RequestUsageJson: [][]byte{[]byte("{not json"), mustJSON(t, good)},
	}
	if err := stream.Send(batch); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatal(err)
	}
	_ = stream.CloseSend()

	var sawMalformed, sawSinkError bool
	for _, rec := range logs.records(t) {
		switch rec["msg"] {
		case "hub skipped malformed usage event":
			sawMalformed = true
			if rec["level"] != "WARN" || fmt.Sprint(rec["batchId"]) != "42" || rec["event"] != "request_usage" {
				t.Errorf("malformed-event line = %v", rec)
			}
			if _, ok := rec["error"]; !ok {
				t.Errorf("malformed-event line carries no error: %v", rec)
			}
		case "hub log sink write failed":
			sawSinkError = true
			if rec["level"] != "ERROR" || fmt.Sprint(rec["batchId"]) != "42" || fmt.Sprint(rec["events"]) != "1" {
				t.Errorf("sink-error line = %v", rec)
			}
			if errText, _ := rec["error"].(string); !strings.Contains(errText, "sink boom") {
				t.Errorf("sink-error line = %v, want the sink's error", rec)
			}
		}
	}
	if !sawMalformed {
		t.Errorf("no malformed-event line in:\n%s", logs.raw())
	}
	if !sawSinkError {
		t.Errorf("no sink-error line in:\n%s", logs.raw())
	}
}

func TestIngestMultipleBatches(t *testing.T) {
	client, sink, cleanup := newIngestClient(t, optionsWithKey(t))
	defer cleanup()

	stream, _ := client.Ingest(context.Background())
	for i := int64(1); i <= 3; i++ {
		ru := usage.RequestUsage{RequestID: "r"}
		if err := stream.Send(&api.IngestBatch{BatchId: i, RequestUsageJson: [][]byte{mustJSON(t, ru)}}); err != nil {
			t.Fatal(err)
		}
		ack, err := stream.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if ack.GetBatchId() != i {
			t.Fatalf("ack = %d, want %d", ack.GetBatchId(), i)
		}
	}
	_ = stream.CloseSend()

	reqs, _ := sink.snapshot()
	if len(reqs) != 3 {
		t.Fatalf("delivered %d events, want 3", len(reqs))
	}
}
