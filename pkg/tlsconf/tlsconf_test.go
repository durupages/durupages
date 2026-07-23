// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package tlsconf

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ca is a throwaway certificate authority for the tests.
type ca struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newCA(t *testing.T, commonName string) *ca {
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
	return &ca{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// issue returns PEM cert/key for a server certificate valid for host.
func (c *ca) issue(t *testing.T, host string) (certPEM, keyPEM []byte) {
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
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
}

func writeFile(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestClientConfigRejectsUnknownCA(t *testing.T) {
	good, other := newCA(t, "good"), newCA(t, "other")
	certPEM, keyPEM := good.issue(t, "example.test")
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{pair}}
	srv.StartTLS()
	defer srv.Close()

	cfg, err := ClientConfig(ClientOptions{CAPEM: other.pem, ServerName: "example.test"})
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}
	if _, err := c.Get(srv.URL); err == nil {
		t.Fatal("a certificate from an untrusted CA was accepted")
	}
}

// cert-manager renews by rewriting the mounted Secret in place, so a listener
// that only read its key pair at startup would keep serving the old
// certificate until it expired.
func TestServerConfigPicksUpRotatedCertificate(t *testing.T) {
	noStatDelay(t)
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")

	first := newCA(t, "first")
	certPEM, keyPEM := first.issue(t, "example.test")
	writeFile(t, certFile, certPEM)
	writeFile(t, keyFile, keyPEM)

	cfg, err := ServerConfig(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	got, err := cfg.GetCertificate(&tls.ClientHelloInfo{ServerName: "example.test"})
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(got.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if leaf.Issuer.CommonName != "first" {
		t.Fatalf("issuer = %q, want first", leaf.Issuer.CommonName)
	}

	// Rotate, then defeat the stat interval the way elapsed time would.
	second := newCA(t, "second")
	certPEM2, keyPEM2 := second.issue(t, "example.test")
	writeFile(t, certFile, certPEM2)
	writeFile(t, keyFile, keyPEM2)
	future := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(certFile, future, future); err != nil {
		t.Fatal(err)
	}

	got, err = cfg.GetCertificate(&tls.ClientHelloInfo{ServerName: "example.test"})
	if err != nil {
		t.Fatal(err)
	}
	leaf, err = x509.ParseCertificate(got.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if leaf.Issuer.CommonName != "second" {
		t.Fatalf("issuer = %q, want second: the rotated certificate was not picked up", leaf.Issuer.CommonName)
	}
}

// A CA mounted from a Secret can be rotated too, and tls.Config offers no way
// to swap RootCAs, so the reloading path has to be exercised end to end.
func TestClientConfigPicksUpRotatedCA(t *testing.T) {
	noStatDelay(t)
	dir := t.TempDir()
	caFile := filepath.Join(dir, "ca.crt")

	old, current := newCA(t, "old"), newCA(t, "current")
	writeFile(t, caFile, old.pem)

	cfg, err := ClientConfig(ClientOptions{CAFile: caFile, ServerName: "example.test"})
	if err != nil {
		t.Fatal(err)
	}

	// A server holding a certificate from the NEW CA is rejected while the file
	// still names the old one...
	certPEM, keyPEM := current.issue(t, "example.test")
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{pair}}
	srv.StartTLS()
	defer srv.Close()

	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: cfg,
		DialContext:     dialTo(srv.Listener.Addr().String()),
	}}
	if _, err := client.Get("https://example.test/"); err == nil {
		t.Fatal("the new CA was trusted before the file was rotated")
	}

	// ...and accepted once it is rotated in.
	writeFile(t, caFile, current.pem)
	future := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(caFile, future, future); err != nil {
		t.Fatal(err)
	}

	client.CloseIdleConnections()
	resp, err := client.Get("https://example.test/")
	if err != nil {
		t.Fatalf("the rotated CA was not picked up: %v", err)
	}
	resp.Body.Close()
}

func TestClientConfigRequiresServerNameWithCAFile(t *testing.T) {
	dir := t.TempDir()
	caFile := filepath.Join(dir, "ca.crt")
	writeFile(t, caFile, newCA(t, "x").pem)

	if _, err := ClientConfig(ClientOptions{CAFile: caFile}); err == nil {
		t.Fatal("a CAFile without a ServerName was accepted, which would skip the hostname check")
	}
}

func TestEnabled(t *testing.T) {
	if (ClientOptions{}).Enabled() {
		t.Error("empty options should mean plaintext")
	}
	if !(ClientOptions{CAPEM: []byte("x")}).Enabled() {
		t.Error("inline CA should enable TLS")
	}
	if !(ClientOptions{InsecureSkipVerify: true}).Enabled() {
		t.Error("skip-verify should enable TLS")
	}
}

func TestHostFromTarget(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"hub.ns.svc.cluster.local:9080", "hub.ns.svc.cluster.local"},
		{"http://hub.ns.svc:9080/v1/x", "hub.ns.svc"},
		{"https://hub.example.com", "hub.example.com"},
		{"10.0.0.5:9440", "10.0.0.5"},
		{"hub", "hub"},
	} {
		if got := HostFromTarget(tt.in); got != tt.want {
			t.Errorf("HostFromTarget(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// noStatDelay makes every lookup re-stat, so a rotation is observed without
// waiting out the interval that keeps a busy listener from stat'ing per
// handshake.
func noStatDelay(t *testing.T) {
	t.Helper()
	prev := statInterval
	statInterval = 0
	t.Cleanup(func() { statInterval = prev })
}

// dialTo redirects every dial to addr, so a request to a name the certificate
// covers reaches the test server regardless of DNS.
func dialTo(addr string) func(ctx context.Context, network, _ string) (net.Conn, error) {
	return func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
}
