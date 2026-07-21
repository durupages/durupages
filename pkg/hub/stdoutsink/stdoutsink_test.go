// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package stdoutsink

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/durupages/durupages/pkg/usage"
)

func TestWriteRequestUsage(t *testing.T) {
	var buf bytes.Buffer
	s := New(&buf)
	ts := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	events := []usage.RequestUsage{
		{RequestID: "r1", TenantID: "t", PageID: "p", DeploymentID: "d", Timestamp: ts},
		{RequestID: "r2", TenantID: "t", PageID: "p", DeploymentID: "d", Timestamp: ts},
	}
	if err := s.WriteRequestUsage(context.Background(), events); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &m); err != nil {
		t.Fatal(err)
	}
	if m["type"] != "request_usage" {
		t.Errorf("type = %v, want request_usage", m["type"])
	}
	if m["requestId"] != "r1" {
		t.Errorf("requestId = %v, want r1", m["requestId"])
	}
	// The wrapped usage fields must be inlined at top level, not nested.
	if _, nested := m["RequestUsage"]; nested {
		t.Error("usage fields should be inlined, not nested under RequestUsage")
	}
}

func TestWriteStaticAccess(t *testing.T) {
	var buf bytes.Buffer
	s := New(&buf)
	events := []usage.StaticAccess{{TenantID: "t", PageID: "p", DeploymentID: "d", BytesSent: 128}}
	if err := s.WriteStaticAccess(context.Background(), events); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m["type"] != "static_access" {
		t.Errorf("type = %v, want static_access", m["type"])
	}
	if m["bytesSent"].(float64) != 128 {
		t.Errorf("bytesSent = %v, want 128", m["bytesSent"])
	}
}

func TestEmptyDefaultsToStdout(t *testing.T) {
	// New(nil) must not panic and must produce a usable sink.
	s := New(nil)
	if s.enc == nil {
		t.Fatal("encoder is nil")
	}
}

func TestNoOutputForEmptySlices(t *testing.T) {
	var buf bytes.Buffer
	s := New(&buf)
	if err := s.WriteRequestUsage(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteStaticAccess(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Fatalf("expected no output, got %q", buf.String())
	}
}
