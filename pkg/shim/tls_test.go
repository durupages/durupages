// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package shim

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/durupages/durupages/pkg/api"
	"github.com/durupages/durupages/pkg/tlsconf"
)

// The certificate helpers below are deliberately local to this file. pkg/tlsconf
// has equivalents in its own tests, but test helpers are not importable across
// packages and copying twenty lines beats exporting a certificate factory from
// production code.

// testCA is a throwaway certificate authority.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newTestCA(t *testing.T, commonName string) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// issue returns a server key pair valid for host.
func (c *testCA) issue(t *testing.T, host string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{host},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}),
	)
	if err != nil {
		t.Fatal(err)
	}
	return pair
}

// newTLSTestShim builds a Shim with the mandatory fields filled in, so a test
// only states the transport settings it cares about. Run is never started: the
// tests here drive one method directly.
func newTLSTestShim(t *testing.T, opts Options) *Shim {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	opts.TenantID = "acme"
	opts.PodName = "pod-1"
	opts.WorkerJWT = "worker-jwt"
	opts.LeasePubKey = pub
	opts.Runtime = &fakeRuntime{}
	opts.BundleDir = t.TempDir()
	opts.LogWriter = io.Discard
	opts.Logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	opts.ProxyAddr, opts.AssetsAddr = "127.0.0.1:0", "127.0.0.1:0"
	opts.TailAddr, opts.HealthAddr = "127.0.0.1:0", "127.0.0.1:0"

	s, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		for _, ln := range []net.Listener{s.proxyLn, s.assetsLn, s.tailLn, s.healthLn} {
			_ = ln.Close()
		}
	})
	return s
}

// startTLSHub serves bundles over TLS with a certificate for hostname, and
// returns the https:// address to reach it at.
func startTLSHub(t *testing.T, ca *testCA, hostname string) (*hubMux, string) {
	t.Helper()
	hm := &hubMux{}
	srv := httptest.NewUnstartedServer(hm)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{ca.issue(t, hostname)}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return hm, srv.URL
}

// The point of the whole exercise: with the CA the controller injected, the
// shim downloads a bundle from a hub that speaks TLS.
func TestFetchBundleOverTLS(t *testing.T) {
	ca := newTestCA(t, "durupages-ca")
	hm, hubAddr := startTLSHub(t, ca, "hub.test")
	hm.set("acme", "page-1", "dep-1", buildBundleTar(t, bundleSpec{
		tenant: "acme", page: "page-1", dep: "dep-1",
		static: map[string]string{"/index.html": "hello"},
	}))

	// The hub is reached at 127.0.0.1 but its certificate names hub.test --
	// exactly the isolated-network case DURUPAGES_HUB_SERVER_NAME exists for.
	cfg, err := tlsconf.ClientConfig(tlsconf.ClientOptions{CAPEM: ca.pem, ServerName: "hub.test"})
	if err != nil {
		t.Fatal(err)
	}
	s := newTLSTestShim(t, Options{HubAddr: hubAddr, HubTLS: cfg})

	dep, err := s.fetchBundle(context.Background(), "page-1", "dep-1")
	if err != nil {
		t.Fatalf("fetch over TLS: %v", err)
	}
	if dep.manifest == nil || dep.manifest.PageID != "page-1" {
		t.Fatalf("bundle did not unpack: %+v", dep)
	}
	if got := hm.requestIDs(); len(got) != 1 {
		t.Fatalf("hub saw %d requests, want 1", len(got))
	}
}

// A hub whose certificate chains to some other CA must not be trusted, and the
// failure has to name the hub: "which hub did we even talk to" is the first
// question asked when a page starts answering 502.
func TestFetchBundleTLSRejectsUnknownCA(t *testing.T) {
	serving, trusted := newTestCA(t, "serving"), newTestCA(t, "trusted")
	hm, hubAddr := startTLSHub(t, serving, "hub.test")
	hm.set("acme", "page-1", "dep-1", buildBundleTar(t, bundleSpec{tenant: "acme", page: "page-1", dep: "dep-1"}))

	cfg, err := tlsconf.ClientConfig(tlsconf.ClientOptions{CAPEM: trusted.pem, ServerName: "hub.test"})
	if err != nil {
		t.Fatal(err)
	}
	s := newTLSTestShim(t, Options{HubAddr: hubAddr, HubTLS: cfg})

	_, err = s.fetchBundle(context.Background(), "page-1", "dep-1")
	if err == nil {
		t.Fatal("a hub certificate from an untrusted CA was accepted")
	}
	if !strings.Contains(err.Error(), hubAddr) {
		t.Errorf("error does not name the hub, so an operator cannot tell which one failed: %v", err)
	}
}

// The server name has to be verified too, not just the chain: a certificate
// signed by the right CA for the wrong host is still the wrong server.
func TestFetchBundleTLSRejectsWrongServerName(t *testing.T) {
	ca := newTestCA(t, "durupages-ca")
	hm, hubAddr := startTLSHub(t, ca, "hub.test")
	hm.set("acme", "page-1", "dep-1", buildBundleTar(t, bundleSpec{tenant: "acme", page: "page-1", dep: "dep-1"}))

	cfg, err := tlsconf.ClientConfig(tlsconf.ClientOptions{CAPEM: ca.pem, ServerName: "not-the-hub.test"})
	if err != nil {
		t.Fatal(err)
	}
	s := newTLSTestShim(t, Options{HubAddr: hubAddr, HubTLS: cfg})

	if _, err := s.fetchBundle(context.Background(), "page-1", "dep-1"); err == nil {
		t.Fatal("a certificate issued for another host was accepted")
	}
}

// Nothing configured must behave exactly as it did before TLS existed.
func TestFetchBundlePlaintextUnchanged(t *testing.T) {
	hm := &hubMux{}
	srv := httptest.NewServer(hm)
	defer srv.Close()
	hm.set("acme", "page-1", "dep-1", buildBundleTar(t, bundleSpec{tenant: "acme", page: "page-1", dep: "dep-1"}))

	s := newTLSTestShim(t, Options{HubAddr: srv.URL})
	if s.httpClient != http.DefaultClient {
		t.Error("no TLS and no HTTPClient should leave the default client in place")
	}
	if _, err := s.fetchBundle(context.Background(), "page-1", "dep-1"); err != nil {
		t.Fatalf("plaintext fetch: %v", err)
	}
}

// HubTLS must not reach into a client the caller (or the standard library)
// shares with anyone else.
func TestHubTLSDoesNotMutateSuppliedClient(t *testing.T) {
	ca := newTestCA(t, "durupages-ca")
	cfg, err := tlsconf.ClientConfig(tlsconf.ClientOptions{CAPEM: ca.pem, ServerName: "hub.test"})
	if err != nil {
		t.Fatal(err)
	}
	base := &http.Transport{MaxIdleConnsPerHost: 7}
	client := &http.Client{Transport: base, Timeout: 42 * time.Second}

	s := newTLSTestShim(t, Options{HubAddr: "https://hub.test", HTTPClient: client, HubTLS: cfg})

	// Identity, not nil-ness: http.Transport.Clone materializes a default
	// TLSClientConfig on the transport it is called on, so the check that means
	// anything is that our verification policy did not end up installed on a
	// transport somebody else is using.
	if base.TLSClientConfig == cfg {
		t.Error("the caller's transport was given the shim's TLS config")
	}
	if http.DefaultTransport.(*http.Transport).TLSClientConfig == cfg {
		t.Fatal("http.DefaultTransport was given the shim's TLS config, which changes TLS for the whole process")
	}
	if s.httpClient == client {
		t.Fatal("the caller's client was reused instead of cloned")
	}
	if s.httpClient.Timeout != 42*time.Second {
		t.Errorf("clone lost the client settings: timeout = %v", s.httpClient.Timeout)
	}
	tr, ok := s.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T, want *http.Transport", s.httpClient.Transport)
	}
	if tr.TLSClientConfig != cfg {
		t.Error("the TLS config was not attached to the clone")
	}
	if tr.MaxIdleConnsPerHost != 7 {
		t.Error("clone lost the transport settings")
	}
}

// Silently ignoring HubTLS would leave the shim trusting whatever the supplied
// transport trusts, which is the opposite of what the caller asked for.
func TestHubTLSRejectsUnknownTransport(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, nil })}
	_, err := New(Options{
		TenantID: "acme", Runtime: &fakeRuntime{}, BundleDir: t.TempDir(),
		LeasePubKey: make(ed25519.PublicKey, ed25519.PublicKeySize),
		HTTPClient:  client, HubTLS: &tls.Config{},
	})
	if err == nil {
		t.Fatal("HubTLS with an unusable transport was accepted")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// startTLSController runs the WorkerService over TLS and returns its address.
func startTLSController(t *testing.T, ca *testCA, hostname string, svc api.WorkerServiceServer) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{ca.issue(t, hostname)},
	})))
	api.RegisterWorkerServiceServer(srv, svc)
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

// The controller hop is gRPC, so it goes through credentials.NewTLS rather than
// an http.Transport; it needs its own end-to-end proof.
func TestControllerDialOverTLS(t *testing.T) {
	ca := newTestCA(t, "durupages-ca")
	svc := &fakeWorkerService{}
	svc.setPageConfig("page-1", &api.GetPageConfigResponse{
		Env:    map[string]string{"MODE": "prod"},
		Secret: map[string]string{"TOKEN": "s3cret"},
	})
	addr := startTLSController(t, ca, "controller.test", svc)

	cfg, err := tlsconf.ClientConfig(tlsconf.ClientOptions{CAPEM: ca.pem, ServerName: "controller.test"})
	if err != nil {
		t.Fatal(err)
	}
	s := newTLSTestShim(t, Options{ControllerAddr: addr, ControllerTLS: cfg})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dep := &deployment{pageID: "page-1", deploymentID: "dep-1"}
	if err := s.fetchPageConfig(ctx, dep); err != nil {
		t.Fatalf("page config over TLS: %v", err)
	}
	if dep.env["MODE"] != "prod" || dep.secret["TOKEN"] != "s3cret" {
		t.Fatalf("bindings did not arrive: env=%v secret=%v", dep.env, dep.secret)
	}
}

func TestControllerDialTLSRejectsUnknownCA(t *testing.T) {
	serving, trusted := newTestCA(t, "serving"), newTestCA(t, "trusted")
	svc := &fakeWorkerService{}
	svc.setPageConfig("page-1", &api.GetPageConfigResponse{Env: map[string]string{"MODE": "prod"}})
	addr := startTLSController(t, serving, "controller.test", svc)

	cfg, err := tlsconf.ClientConfig(tlsconf.ClientOptions{CAPEM: trusted.pem, ServerName: "controller.test"})
	if err != nil {
		t.Fatal(err)
	}
	s := newTLSTestShim(t, Options{ControllerAddr: addr, ControllerTLS: cfg})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dep := &deployment{pageID: "page-1", deploymentID: "dep-1"}
	if err := s.fetchPageConfig(ctx, dep); err == nil {
		t.Fatal("a controller certificate from an untrusted CA was accepted")
	}
}

// An injected WorkerClient brings its own connection, so ControllerTLS must not
// make the shim dial anyway.
func TestControllerTLSIgnoredWithInjectedClient(t *testing.T) {
	svc := &fakeWorkerService{}
	svc.setPageConfig("page-1", &api.GetPageConfigResponse{Env: map[string]string{"MODE": "prod"}})
	s := newTLSTestShim(t, Options{
		ControllerAddr: "127.0.0.1:1", // would fail if it were dialled
		ControllerTLS:  &tls.Config{ServerName: "controller.test"},
		WorkerClient:   newWorkerClient(t, svc),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dep := &deployment{pageID: "page-1", deploymentID: "dep-1"}
	if err := s.fetchPageConfig(ctx, dep); err != nil {
		t.Fatalf("injected client should still be used: %v", err)
	}
}

// The failure this logging exists for: with TLS enabled on the controller but
// the worker unable to complete the handshake, the pod never registers, so it
// is never given a slot, and the only thing an operator sees is the router
// reporting "worker slot queue timeout" -- which points at capacity. The cause
// has to be visible on the worker side.
func TestControllerRegistrationFailureIsLogged(t *testing.T) {
	h, _ := newHarness(t, harnessOpts{noStartRun: true})

	// A listener that accepts and immediately closes stands in for any dial
	// that cannot become a working gRPC session -- a TLS mismatch included.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	go func() {
		for {
			c, aerr := lis.Accept()
			if aerr != nil {
				return
			}
			c.Close()
		}
	}()

	h.shim.opts.ControllerAddr = lis.Addr().String()
	h.shim.opts.ControllerTLS = &tls.Config{MinVersion: tls.VersionTLS12}

	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(credentials.NewTLS(h.shim.opts.ControllerTLS)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h.shim.register(ctx, api.NewWorkerServiceClient(conn))

	var found map[string]any
	for _, l := range h.opsLines() {
		if l["msg"] == logMsgRegisterFailed {
			found = l
		}
	}
	if found == nil {
		t.Fatalf("registration failed silently; nothing explains why the pod never took traffic.\nlines: %v", h.opsLines())
	}
	if found["level"] != "ERROR" {
		t.Errorf("level = %v, want ERROR on the final attempt", found["level"])
	}
	if found["tls"] != true {
		t.Errorf("tls = %v, want true so a handshake problem is distinguishable from a plaintext one", found["tls"])
	}
	if found["controllerAddr"] != lis.Addr().String() {
		t.Errorf("controllerAddr = %v, want the address actually dialled", found["controllerAddr"])
	}
	if e, _ := found["error"].(string); e == "" {
		t.Error("the underlying error was dropped")
	}
}
