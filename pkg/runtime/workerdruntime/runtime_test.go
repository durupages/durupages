// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package workerdruntime

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/durupages/durupages/pkg/runtime"
)

// fakeWorkerdSrc is a stand-in for workerd used by lifecycle tests. It parses
// the entry socket address out of config.capnp, dumps its environment for
// assertions, serves HTTP 200 on every path, and exits on SIGTERM unless a
// "stubborn" marker exists in the parent (WorkDir) directory.
const fakeWorkerdSrc = `package main

import (
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
)

func main() {
	cfg, err := os.ReadFile("config.capnp")
	if err != nil { os.Exit(2) }
	m := regexp.MustCompile(` + "`" + `name = "http", address = "([^"]+)"` + "`" + `).FindSubmatch(cfg)
	if m == nil { os.Exit(3) }
	addr := string(m[1])

	os.WriteFile("env.dump", []byte(strings.Join(os.Environ(), "\n")), 0644)

	ln, err := net.Listen("tcp", addr)
	if err != nil { os.Exit(4) }

	if _, err := os.Stat(filepath("stubborn")); err == nil {
		signal.Ignore(syscall.SIGTERM)
	} else {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM)
		go func() { <-ch; os.Exit(0) }()
	}

	http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("ok"))
	}))
}

func filepath(name string) string { return ".." + string(os.PathSeparator) + name }
`

// buildFakeWorkerd compiles fakeWorkerdSrc into a binary and returns its path.
func buildFakeWorkerd(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte(fakeWorkerdSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "workerd-fake")
	cmd := exec.Command("go", "build", "-o", bin, src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fake workerd: %v\n%s", err, out)
	}
	return bin
}

func lifecycleSpec(t *testing.T) runtime.InstanceSpec {
	dir := writeBundle(t, filepath.Join(t.TempDir(), "blog"), map[string]string{
		"worker/index.js": "export default { async fetch() { return new Response('ok'); } }\n",
	})
	return runtime.InstanceSpec{
		AssetsEndpoint: "127.0.0.1:8081",
		TailEndpoint:   "127.0.0.1:8082",
		Pages: []runtime.PageWorker{
			{PageID: "blog", DeploymentID: "dep1", BundleDir: dir},
		},
	}
}

func TestLaunchWaitReadyClose(t *testing.T) {
	bin := buildFakeWorkerd(t)
	workDir := t.TempDir()
	var stderr bytes.Buffer
	rt := New(Options{WorkerdBin: bin, WorkDir: workDir, Stderr: &stderr})

	// A sentinel that must NOT leak into the cleared child environment.
	t.Setenv("DURUPAGES_SENTINEL", "leak-me")

	ctx := context.Background()
	inst, err := rt.Launch(ctx, lifecycleSpec(t))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer inst.Close()

	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := inst.WaitReady(wctx); err != nil {
		t.Fatalf("WaitReady: %v\nstderr:\n%s", err, stderr.String())
	}

	// The instance serves requests.
	resp, err := http.Get("http://" + inst.Endpoint() + "/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	// The child environment was cleared: PATH present, sentinel absent.
	assertEnvCleared(t, workDir)

	if err := inst.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	// Close is idempotent.
	if err := inst.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func assertEnvCleared(t *testing.T, workDir string) {
	t.Helper()
	entries, err := os.ReadDir(workDir)
	if err != nil {
		t.Fatal(err)
	}
	var dump []byte
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "inst-") {
			b, rerr := os.ReadFile(filepath.Join(workDir, e.Name(), "env.dump"))
			if rerr == nil {
				dump = b
				break
			}
		}
	}
	if dump == nil {
		t.Fatal("no env.dump found")
	}
	env := string(dump)
	if strings.Contains(env, "DURUPAGES_SENTINEL") {
		t.Errorf("child env leaked sentinel:\n%s", env)
	}
	if !strings.Contains(env, "PATH=") {
		t.Errorf("child env missing PATH:\n%s", env)
	}
}

// TestCloseEscalatesToKill verifies Close escalates to SIGKILL when the process
// ignores SIGTERM.
func TestCloseEscalatesToKill(t *testing.T) {
	bin := buildFakeWorkerd(t)
	workDir := t.TempDir()
	// Marker makes the fake ignore SIGTERM.
	if err := os.WriteFile(filepath.Join(workDir, "stubborn"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	old := killGrace
	killGrace = 200 * time.Millisecond
	defer func() { killGrace = old }()

	rt := New(Options{WorkerdBin: bin, WorkDir: workDir, Stderr: &bytes.Buffer{}})
	inst, err := rt.Launch(context.Background(), lifecycleSpec(t))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	wctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := inst.WaitReady(wctx); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}

	done := make(chan struct{})
	go func() { inst.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not escalate to SIGKILL in time")
	}
}

// TestDrainWaitsForInFlight verifies Drain blocks until the in-flight func hits 0.
func TestDrainWaitsForInFlight(t *testing.T) {
	bin := buildFakeWorkerd(t)
	rt := New(Options{WorkerdBin: bin, WorkDir: t.TempDir(), Stderr: &bytes.Buffer{}})
	inst, err := rt.Launch(context.Background(), lifecycleSpec(t))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer inst.Close()

	var mu sync.Mutex
	n := 2
	inst.(interface{ SetInFlightFunc(func() int) }).SetInFlightFunc(func() int {
		mu.Lock()
		defer mu.Unlock()
		return n
	})

	drained := make(chan error, 1)
	go func() { drained <- inst.Drain(context.Background()) }()

	select {
	case <-drained:
		t.Fatal("Drain returned while in-flight > 0")
	case <-time.After(100 * time.Millisecond):
	}

	mu.Lock()
	n = 0
	mu.Unlock()

	select {
	case err := <-drained:
		if err != nil {
			t.Errorf("Drain: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Drain did not return after in-flight reached 0")
	}
}

// TestIntegrationRealWorkerd runs against a real workerd if DURUPAGES_WORKERD_BIN
// points at an existing binary; otherwise it is skipped.
func TestIntegrationRealWorkerd(t *testing.T) {
	bin := os.Getenv("DURUPAGES_WORKERD_BIN")
	if bin == "" {
		t.Skip("DURUPAGES_WORKERD_BIN not set")
	}
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("workerd binary not found: %v", err)
	}
	rt := New(Options{WorkerdBin: bin, WorkDir: t.TempDir()})
	inst, err := rt.Launch(context.Background(), lifecycleSpec(t))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer inst.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := inst.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	req, _ := http.NewRequest(http.MethodGet, "http://"+inst.Endpoint()+"/", nil)
	req.Header.Set("X-DuruPages-Page", "blog")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}
