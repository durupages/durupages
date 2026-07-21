// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

// Package workerdruntime is the default runtime.Runtime implementation: it
// drives a workerd-family binary (durupages-workerd, or a stock workerd in
// development). For each load set it generates a config.capnp plus a trusted
// entry dispatcher and tail worker, then spawns `workerd serve config.capnp`
// with a cleared environment and, optionally, a dropped uid/gid.
//
// This package deliberately does NOT implement runtime.MetricsSource: workerd
// does not push per-request traces to the runtime. Instead the generated tail
// worker POSTs traces to the shim's tail collector, which correlates them into
// usage events. The runtime only manages the process lifecycle.
package workerdruntime

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/durupages/durupages/pkg/runtime"
)

// compile-time check that *Runtime satisfies the extension point.
var _ runtime.Runtime = (*Runtime)(nil)

// killGrace is how long Close waits after SIGTERM before escalating to SIGKILL.
// It is a package variable so tests can shorten it.
var killGrace = 5 * time.Second

// Options configures a Runtime.
type Options struct {
	// WorkerdBin is the workerd binary path. When empty it falls back to the
	// DURUPAGES_WORKERD_BIN environment variable and then to "workerd".
	WorkerdBin string
	// WorkDir is the parent directory for per-instance directories.
	WorkDir string
	// DropToUID/DropToGID, when > 0, run workerd under that uid/gid via a
	// SysProcAttr credential. This requires the parent to hold CAP_SETUID /
	// CAP_SETGID; if the syscall fails with EPERM the runtime automatically
	// retries at the same uid and logs a warning.
	DropToUID int
	DropToGID int
	// Stderr receives workerd's stdout/stderr, line-prefixed with "[workerd]".
	// Defaults to os.Stderr.
	Stderr io.Writer
}

// Runtime launches workerd instances.
type Runtime struct {
	bin    string
	dir    string
	uid    int
	gid    int
	stderr io.Writer
}

// New returns a Runtime from opts.
func New(opts Options) *Runtime {
	bin := opts.WorkerdBin
	if bin == "" {
		if env := os.Getenv("DURUPAGES_WORKERD_BIN"); env != "" {
			bin = env
		} else {
			bin = "workerd"
		}
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	return &Runtime{
		bin:    bin,
		dir:    opts.WorkDir,
		uid:    opts.DropToUID,
		gid:    opts.DropToGID,
		stderr: stderr,
	}
}

// Launch writes the config and generated workers into a fresh per-instance
// directory, spawns workerd and returns the (not-yet-ready) instance.
func (r *Runtime) Launch(ctx context.Context, spec runtime.InstanceSpec) (runtime.Instance, error) {
	port, err := freePort()
	if err != nil {
		return nil, fmt.Errorf("workerdruntime: allocate port: %w", err)
	}

	if r.dir != "" {
		if err := os.MkdirAll(r.dir, 0o700); err != nil {
			return nil, fmt.Errorf("workerdruntime: workdir: %w", err)
		}
	}
	dir, err := os.MkdirTemp(r.dir, "inst-")
	if err != nil {
		return nil, fmt.Errorf("workerdruntime: instance dir: %w", err)
	}

	gen, err := generateConfig(spec, port)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}

	// Write generated artifacts.
	if err := writeFiles(dir, map[string][]byte{
		"config.capnp": gen.Capnp,
		entryModule:    gen.EntryJS,
		tailModule:     gen.TailJS,
	}); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	// Symlink each page's bundle dir so the config's `embed page_<i>/...` paths
	// resolve. Deployments are immutable so the link target is stable.
	for i, p := range spec.Pages {
		link := filepath.Join(dir, fmt.Sprintf("page_%d", i))
		if err := os.Symlink(p.BundleDir, link); err != nil {
			_ = os.RemoveAll(dir)
			return nil, fmt.Errorf("workerdruntime: link bundle: %w", err)
		}
	}

	endpoint := fmt.Sprintf("127.0.0.1:%d", port)
	inst := &instance{
		endpoint:  endpoint,
		dir:       dir,
		killGrace: killGrace,
		done:      make(chan struct{}),
	}
	if err := r.start(inst); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return inst, nil
}

// start spawns workerd for inst, wiring output capture and the optional uid/gid
// drop with an automatic EPERM fallback.
func (r *Runtime) start(inst *instance) error {
	cmd := exec.Command(r.bin, "serve", "config.capnp")
	cmd.Dir = inst.dir
	// Cleared environment: only a minimal PATH so the binary and its loader
	// resolve. No inherited env reaches the untrusted worker process.
	cmd.Env = []string{"PATH=" + minimalPath()}

	if r.uid > 0 || r.gid > 0 {
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Credential: &syscall.Credential{Uid: uint32(r.uid), Gid: uint32(r.gid)},
		}
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}

	if err := cmd.Start(); err != nil {
		// CAP_SETUID/SETGID missing: fall back to the same uid rather than
		// failing the whole pod, and warn.
		if r.uid > 0 && errors.Is(err, syscall.EPERM) {
			fmt.Fprintf(r.stderr, "[workerd] warning: cannot drop to uid %d (EPERM); running at current uid\n", r.uid)
			cmd.SysProcAttr = nil
			// StdoutPipe/StderrPipe are single-use; rebuild the command.
			cmd = exec.Command(r.bin, "serve", "config.capnp")
			cmd.Dir = inst.dir
			cmd.Env = []string{"PATH=" + minimalPath()}
			if stdout, err = cmd.StdoutPipe(); err != nil {
				return err
			}
			if stderr, err = cmd.StderrPipe(); err != nil {
				return err
			}
			if err = cmd.Start(); err != nil {
				return err
			}
		} else {
			return err
		}
	}

	inst.cmd = cmd
	go pipePrefixed(r.stderr, stdout)
	go pipePrefixed(r.stderr, stderr)
	go func() {
		inst.waitErr = cmd.Wait()
		close(inst.done)
	}()
	return nil
}

// instance is a running workerd process.
type instance struct {
	endpoint  string
	dir       string
	cmd       *exec.Cmd
	killGrace time.Duration
	done      chan struct{}
	waitErr   error

	mu       sync.Mutex
	inFlight func() int
	closed   bool
}

// Endpoint returns the loopback address requests are proxied to.
func (i *instance) Endpoint() string { return i.endpoint }

// SetInFlightFunc installs the shim-provided in-flight counter. Drain waits for
// it to reach zero. The shim tracks in-flight requests per instance in its
// proxy, so the runtime does not count them itself.
func (i *instance) SetInFlightFunc(f func() int) {
	i.mu.Lock()
	i.inFlight = f
	i.mu.Unlock()
}

// WaitReady polls the entry socket until a TCP connect plus a HEAD both
// succeed, the context is cancelled, or the process exits.
func (i *instance) WaitReady(ctx context.Context) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	client := &http.Client{Timeout: 500 * time.Millisecond}
	for {
		if i.probe(ctx, client) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-i.done:
			return fmt.Errorf("workerdruntime: process exited before ready: %v", i.waitErr)
		case <-ticker.C:
		}
	}
}

func (i *instance) probe(ctx context.Context, client *http.Client) bool {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", i.endpoint)
	if err != nil {
		return false
	}
	_ = conn.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, "http://"+i.endpoint+"/", nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return true
}

// Drain waits for the shim-tracked in-flight count to reach zero, or ctx done.
func (i *instance) Drain(ctx context.Context) error {
	i.mu.Lock()
	f := i.inFlight
	i.mu.Unlock()
	if f == nil {
		return nil
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if f() <= 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-i.done:
			return nil
		case <-ticker.C:
		}
	}
}

// Close terminates workerd (SIGTERM, then SIGKILL after killGrace) and removes
// the instance directory. It is idempotent.
func (i *instance) Close() error {
	i.mu.Lock()
	if i.closed {
		i.mu.Unlock()
		return nil
	}
	i.closed = true
	i.mu.Unlock()

	if i.cmd != nil && i.cmd.Process != nil {
		_ = i.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-i.done:
		case <-time.After(i.killGrace):
			_ = i.cmd.Process.Kill()
			<-i.done
		}
	}
	if i.dir != "" {
		_ = os.RemoveAll(i.dir)
	}
	return nil
}

// freePort asks the OS for an unused TCP port on the loopback interface.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// minimalPath returns a minimal PATH for the workerd child: the parent PATH if
// set (so a bare "workerd" resolves), else a conservative default.
func minimalPath() string {
	if p := os.Getenv("PATH"); p != "" {
		return p
	}
	return "/usr/local/bin:/usr/bin:/bin"
}

// writeFiles writes each name->content into dir with 0600 permissions.
func writeFiles(dir string, files map[string][]byte) error {
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// pipePrefixed copies r to w line by line, prefixing each line with "[workerd]".
func pipePrefixed(w io.Writer, r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		fmt.Fprintf(w, "[workerd] %s\n", sc.Text())
	}
}
