// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package shim

import (
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/durupages/durupages/pkg/runtime"
)

// TestCollectorRejectsLargeBody verifies the tail collector rejects bodies over
// 1 MiB.
func TestCollectorRejectsLargeBody(t *testing.T) {
	h, _ := newHarness(t, harnessOpts{})
	big := bytes.Repeat([]byte("a"), maxTailBody+10)
	resp, err := http.Post("http://"+h.shim.TailAddr()+"/", "application/json", bytes.NewReader(big))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", resp.StatusCode)
	}
}

// TestCollectorRejectsNonPost verifies non-POST methods are rejected.
func TestCollectorRejectsNonPost(t *testing.T) {
	h, _ := newHarness(t, harnessOpts{})
	resp, err := http.Get("http://" + h.shim.TailAddr() + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
}

// TestCorrelatorExpectWaitDeliver exercises the correlator directly.
func TestCorrelatorExpectWaitDeliver(t *testing.T) {
	c := newCorrelator(time.Now)
	c.expect("rid")
	go c.deliver(runtime.RequestTrace{RequestID: "rid", CPUTime: 5 * time.Millisecond})
	tr := c.wait(context.Background(), "rid", time.Second)
	if tr == nil || tr.CPUTime != 5*time.Millisecond {
		t.Fatalf("trace = %+v", tr)
	}
	// Entry is forgotten after wait.
	if tr2 := c.wait(context.Background(), "rid", 10*time.Millisecond); tr2 != nil {
		t.Errorf("expected nil after forget, got %+v", tr2)
	}
}
