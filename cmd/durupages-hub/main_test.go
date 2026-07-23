// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/durupages/durupages/pkg/api"
	"github.com/durupages/durupages/pkg/hub"
	"github.com/durupages/durupages/pkg/storage"
	"github.com/durupages/durupages/pkg/storage/memstorage"
	"github.com/durupages/durupages/pkg/tlsconf"
	"github.com/durupages/durupages/pkg/usage"
	"github.com/durupages/durupages/pkg/workerauth"
)

const (
	testTenant     = "tenant-a"
	testPage       = "page-1"
	testDeployment = "dep-xyz"
)

var testBundle = []byte("bundle-bytes")

// testCA is a throwaway certificate authority for these tests. The tests mint
// their own material rather than sharing pkg/tlsconf's helpers so that this
// package's TLS wiring is verified against nothing but the exported API.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newCA(t *testing.T, commonName string) *testCA {
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

// issue writes a server key pair for host into dir and returns the two paths,
// the way a mounted cert-manager Secret would present them.
func (c *testCA) issue(t *testing.T, dir, host string) tlsFiles {
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
	files := tlsFiles{
		certFile: filepath.Join(dir, host+".crt"),
		keyFile:  filepath.Join(dir, host+".key"),
	}
	writeFile(t, files.certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	writeFile(t, files.keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}))
	return files
}

func writeFile(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// discardSink satisfies hub.LogSink; the ingest tests here assert on the
// transport, not on what the sink receives.
type discardSink struct{}

func (discardSink) WriteRequestUsage(context.Context, []usage.RequestUsage) error { return nil }
func (discardSink) WriteStaticAccess(context.Context, []usage.StaticAccess) error { return nil }

// newTestHub returns a hub holding one bundle, plus the key that signs worker
// tokens for it.
func newTestHub(t *testing.T) (*hub.Hub, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := workerauth.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	store := memstorage.New()
	key := fmt.Sprintf(storage.WorkerBundleKeyFmt, testTenant, testPage, testDeployment)
	if err := store.Put(context.Background(), key, bytes.NewReader(testBundle),
		int64(len(testBundle)), "application/x-tar"); err != nil {
		t.Fatal(err)
	}
	h, err := hub.New(hub.Options{
		Storage:      store,
		JWTPublicKey: pub,
		Sink:         discardSink{},
		Logger:       slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return h, priv
}

// startBundleServer serves the bundle API on a loopback port through the same
// constructor and serve path the binary uses, and returns "host:port".
func startBundleServer(t *testing.T, h *hub.Hub, tlsCfg *tls.Config) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := newBundleServer(lis.Addr().String(), h, tlsCfg)
	go func() { _ = serveHTTP(srv, lis) }()
	t.Cleanup(func() { _ = srv.Close() })
	return lis.Addr().String()
}

// startLogServer serves log ingest on a loopback port, likewise through the
// binary's own constructor.
func startLogServer(t *testing.T, h *hub.Hub, tlsCfg *tls.Config) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := newLogServer(h, tlsCfg)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

func bundleURL(scheme, addr string) string {
	return fmt.Sprintf("%s://%s/v1/tenants/%s/pages/%s/deployments/%s/bundle.tar",
		scheme, addr, testTenant, testPage, testDeployment)
}

func workerToken(t *testing.T, priv ed25519.PrivateKey) string {
	t.Helper()
	tok, err := workerauth.Issue(priv, "pod-1", testTenant, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// getBundle fetches the bundle over the client config, overriding the dial
// target so a certificate issued for a hostname can be exercised on loopback.
func getBundle(t *testing.T, addr, host string, clientTLS *tls.Config, token string) (*http.Response, error) {
	t.Helper()
	tr := &http.Transport{TLSClientConfig: clientTLS}
	if clientTLS == nil {
		tr = &http.Transport{}
	}
	c := &http.Client{Transport: tr, Timeout: 10 * time.Second}
	scheme := "https"
	if clientTLS == nil {
		scheme = "http"
	}
	req, err := http.NewRequest(http.MethodGet, bundleURL(scheme, addr), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	req.Header.Set("Authorization", "Bearer "+token)
	return c.Do(req)
}

// The end-to-end property: a hub started with a certificate serves the bundle
// over TLS to a client that trusts the issuing CA, and serves nothing at all in
// plaintext on that port.
func TestBundleListenerServesTLSToTrustingClient(t *testing.T) {
	dir := t.TempDir()
	ca := newCA(t, "bundle-ca")
	const host = "bundle.durupages.test"
	files := ca.issue(t, dir, host)

	srvTLS, err := serverTLS("bundle HTTP", files)
	if err != nil {
		t.Fatal(err)
	}
	if srvTLS == nil {
		t.Fatal("serverTLS returned no config for a configured certificate pair")
	}
	h, priv := newTestHub(t)
	addr := startBundleServer(t, h, srvTLS)

	clientTLS, err := tlsconf.ClientConfig(tlsconf.ClientOptions{CAPEM: ca.pem, ServerName: host})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := getBundle(t, addr, host, clientTLS, workerToken(t, priv))
	if err != nil {
		t.Fatalf("TLS bundle request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, testBundle) {
		t.Fatalf("body = %q, want %q", body, testBundle)
	}

	// The listener must not also answer plaintext: an operator who configured a
	// certificate has to be able to assume nothing leaves the port in the clear.
	// Go's TLS server answers a plaintext request with a 400 rather than closing
	// the connection, so the check is that no bundle comes back.
	if resp, err := getBundle(t, addr, host, nil, workerToken(t, priv)); err == nil {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK || bytes.Contains(body, testBundle) {
			t.Fatalf("a plaintext request was served (status %d, body %q)", resp.StatusCode, body)
		}
	}

	// And a client trusting a different CA must be refused.
	other := newCA(t, "other-ca")
	otherTLS, err := tlsconf.ClientConfig(tlsconf.ClientOptions{CAPEM: other.pem, ServerName: host})
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := getBundle(t, addr, host, otherTLS, workerToken(t, priv)); err == nil {
		resp.Body.Close()
		t.Fatal("a client trusting an unrelated CA was served")
	}
}

// ingest opens a stream and round-trips one batch, returning the transport
// error if the handshake or the RPC fails.
func ingest(t *testing.T, addr, host string, clientTLS *tls.Config) error {
	t.Helper()
	creds := credentials.NewTLS(clientTLS)
	conn, err := grpc.NewClient("passthrough:///"+addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		return err
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := api.NewLogServiceClient(conn).Ingest(ctx)
	if err != nil {
		return err
	}
	if err := stream.Send(&api.IngestBatch{BatchId: 7}); err != nil {
		return err
	}
	ack, err := stream.Recv()
	if err != nil {
		return err
	}
	if ack.GetBatchId() != 7 {
		t.Fatalf("ack batchId = %d, want 7", ack.GetBatchId())
	}
	return nil
}

// The two listeners are independently certified: the deployment where bundle
// downloads and log ingest answer to different hostnames (and different CAs)
// has to work, so --log-tls-cert-file must not be tangled with --tls-cert-file.
func TestListenersUseSeparateCertificates(t *testing.T) {
	dir := t.TempDir()
	bundleCA, logCA := newCA(t, "bundle-ca"), newCA(t, "log-ca")
	const bundleHost, logHost = "bundle.durupages.test", "logs.durupages.test"
	bundleFiles := bundleCA.issue(t, dir, bundleHost)
	logFiles := logCA.issue(t, dir, logHost)

	bundleTLS, err := serverTLS("bundle HTTP", bundleFiles)
	if err != nil {
		t.Fatal(err)
	}
	logTLS, err := serverTLS("log ingest gRPC", tlsFiles{}.orElse(logFiles))
	if err != nil {
		t.Fatal(err)
	}
	h, priv := newTestHub(t)
	bundleAddr := startBundleServer(t, h, bundleTLS)
	logAddr := startLogServer(t, h, logTLS)

	bundleClient, err := tlsconf.ClientConfig(tlsconf.ClientOptions{CAPEM: bundleCA.pem, ServerName: bundleHost})
	if err != nil {
		t.Fatal(err)
	}
	logClient, err := tlsconf.ClientConfig(tlsconf.ClientOptions{CAPEM: logCA.pem, ServerName: logHost})
	if err != nil {
		t.Fatal(err)
	}

	resp, err := getBundle(t, bundleAddr, bundleHost, bundleClient, workerToken(t, priv))
	if err != nil {
		t.Fatalf("bundle request with the bundle CA failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if err := ingest(t, logAddr, logHost, logClient); err != nil {
		t.Fatalf("ingest with the log CA failed: %v", err)
	}
	// Each listener presents only its own certificate.
	if err := ingest(t, logAddr, logHost, bundleClient); err == nil {
		t.Fatal("the log listener was accepted by a client trusting only the bundle CA")
	}
}

// The log listener reuses the bundle pair when it is given none of its own,
// which is the single-hostname deployment.
func TestLogListenerInheritsBundleCertificate(t *testing.T) {
	dir := t.TempDir()
	ca := newCA(t, "shared-ca")
	const host = "hub.durupages.test"
	files := ca.issue(t, dir, host)

	logTLS, err := serverTLS("log ingest gRPC", tlsFiles{}.orElse(files))
	if err != nil {
		t.Fatal(err)
	}
	if logTLS == nil {
		t.Fatal("the log listener fell back to plaintext instead of the bundle certificate")
	}
	h, _ := newTestHub(t)
	addr := startLogServer(t, h, logTLS)

	clientTLS, err := tlsconf.ClientConfig(tlsconf.ClientOptions{CAPEM: ca.pem, ServerName: host})
	if err != nil {
		t.Fatal(err)
	}
	if err := ingest(t, addr, host, clientTLS); err != nil {
		t.Fatalf("ingest over the inherited certificate failed: %v", err)
	}
}

// Unset flags mean plaintext, and an explicit log pair wins over the bundle one.
func TestTLSFilesResolution(t *testing.T) {
	bundle := tlsFiles{certFile: "b.crt", keyFile: "b.key"}
	logPair := tlsFiles{certFile: "l.crt", keyFile: "l.key"}

	if got := (tlsFiles{}).orElse(bundle); got != bundle {
		t.Fatalf("unset log pair resolved to %+v, want the bundle pair", got)
	}
	if got := logPair.orElse(bundle); got != logPair {
		t.Fatalf("explicit log pair resolved to %+v, want itself", got)
	}
	cfg, err := serverTLS("bundle HTTP", tlsFiles{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg != nil {
		t.Fatal("no certificate configured, yet a TLS config was built")
	}
}

// Startup must fail loudly on an unusable certificate. Falling back to
// plaintext here would leave the operator believing the hop is encrypted while
// bundles and worker tokens cross the network in the clear.
func TestRunFailsOnUnusableCertificate(t *testing.T) {
	dir := t.TempDir()
	ca := newCA(t, "hub-ca")
	good := ca.issue(t, dir, "hub.durupages.test")
	otherPair := ca.issue(t, dir, "other.durupages.test")

	missing := tlsFiles{certFile: filepath.Join(dir, "absent.crt"), keyFile: filepath.Join(dir, "absent.key")}
	mismatched := tlsFiles{certFile: good.certFile, keyFile: otherPair.keyFile}

	cases := []struct {
		name    string
		cfg     config
		wantErr string
	}{
		{"bundle certificate missing", config{httpTLS: missing}, "bundle HTTP TLS"},
		{"bundle key omitted", config{httpTLS: tlsFiles{certFile: good.certFile}}, "must both be set"},
		{"bundle pair mismatched", config{httpTLS: mismatched}, "bundle HTTP TLS"},
		{"log certificate missing", config{httpTLS: good, logTLS: missing}, "log ingest gRPC TLS"},
		{"log key omitted", config{logTLS: tlsFiles{certFile: good.certFile}}, "must both be set"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A listen address that would fail anyway is not needed: run must
			// reject the configuration before it opens a socket.
			err := run(context.Background(), tc.cfg)
			if err == nil {
				t.Fatal("run succeeded with an unusable certificate")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

// The startup line has to say whether a listener is protected and with which
// files, so a plaintext listener is distinguishable in the log.
func TestListenAttrs(t *testing.T) {
	files := tlsFiles{certFile: "/etc/tls/tls.crt", keyFile: "/etc/tls/tls.key"}
	got := fmt.Sprint(listenAttrs(":9080", files, true)...)
	for _, want := range []string{":9080", "true", files.certFile, files.keyFile} {
		if !strings.Contains(got, want) {
			t.Fatalf("attrs = %s, want it to contain %q", got, want)
		}
	}
	got = fmt.Sprint(listenAttrs(":9080", tlsFiles{}, false)...)
	if !strings.Contains(got, "false") {
		t.Fatalf("attrs = %s, want it to report tls=false", got)
	}
}
