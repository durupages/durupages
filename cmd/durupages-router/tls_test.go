// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc/credentials/insecure"
)

func caPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "durupages-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// flags builds a tlsFlags from plain values, the way flag.Parse would.
func flags(pemStr, file string, skip bool) tlsFlags {
	return tlsFlags{caPEM: &pemStr, caFile: &file, skipVerify: &skip}
}

// TLS is opt-in: the router with no TLS settings dials plaintext exactly as
// before.
func TestRouterClientTLSDisabled(t *testing.T) {
	cfg, err := flags(string(caPEM(t)), "", false).clientTLS("controller:9440", false, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg != nil {
		t.Fatal("a disabled target must stay plaintext even with a CA available")
	}
	if got := grpcCreds(nil).Info().SecurityProtocol; got != insecure.NewCredentials().Info().SecurityProtocol {
		t.Errorf("nil config yielded %q credentials, want insecure", got)
	}
}

// The fail-open counterpart to TestRouterClientTLSDisabled: a hop enabled with
// no CA verifies against the system roots -- RootCAs nil, verification still on,
// not skip-verify. Intentional for publicly-trusted server certs; with an
// internal CA it fails at the handshake, and the controller warns about the
// worker hops at startup. Pinned so the behavior is a deliberate choice.
func TestRouterClientTLSEnabledWithoutCAUsesSystemRoots(t *testing.T) {
	cfg, err := flags("", "", false).clientTLS("controller:9440", true, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg == nil {
		t.Fatal("an enabled hop must produce a TLS config")
	}
	if cfg.RootCAs != nil {
		t.Fatal("no CA configured but RootCAs is set; expected the system-roots fallback")
	}
	if cfg.InsecureSkipVerify {
		t.Fatal("verification must stay on; system roots is not skip-verify")
	}
}

func TestRouterClientTLSDerivesServerName(t *testing.T) {
	f := flags(string(caPEM(t)), "", false)
	for _, tt := range []struct{ target, want string }{
		{"controller.durupages.svc.cluster.local:9440", "controller.durupages.svc.cluster.local"},
		{"10.96.0.7:9440", "10.96.0.7"},
	} {
		cfg, err := f.clientTLS(tt.target, true, "")
		if err != nil {
			t.Fatalf("%s: %v", tt.target, err)
		}
		if cfg.ServerName != tt.want {
			t.Errorf("clientTLS(%q).ServerName = %q, want %q", tt.target, cfg.ServerName, tt.want)
		}
	}
	cfg, err := f.clientTLS("10.96.0.7:9440", true, "controller.durupages.svc")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ServerName != "controller.durupages.svc" {
		t.Errorf("ServerName = %q, want the override", cfg.ServerName)
	}
}

// tlsconf refuses a file CA without a server name, so the derivation is what
// keeps the mounted-Secret deployment working.
func TestRouterClientTLSFileCA(t *testing.T) {
	file := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(file, caPEM(t), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := flags("", file, false).clientTLS("hub.durupages.svc:9080", true, "")
	if err != nil {
		t.Fatalf("file CA rejected: %v", err)
	}
	if cfg.ServerName != "hub.durupages.svc" {
		t.Errorf("ServerName = %q, want the derived name", cfg.ServerName)
	}
}

func TestRouterClientTLSInlinePEMBeatsFile(t *testing.T) {
	f := flags(string(caPEM(t)), filepath.Join(t.TempDir(), "missing.crt"), false)
	cfg, err := f.clientTLS("hub:9080", true, "")
	if err != nil {
		t.Fatalf("the unreadable file was consulted despite inline PEM: %v", err)
	}
	if cfg.RootCAs == nil {
		t.Error("inline PEM should have produced a root pool")
	}
}

func TestRouterClientTLSRejectsGarbageCA(t *testing.T) {
	if _, err := flags("not a certificate", "", false).clientTLS("hub:9080", true, ""); err == nil {
		t.Fatal("garbage CA PEM was accepted, which would silently fall back to the system roots")
	}
}

func TestRouterEnvBool(t *testing.T) {
	const key = "DURUPAGES_TEST_BOOL"
	for _, tt := range []struct {
		value string
		want  bool
	}{{"", false}, {"true", true}, {"1", true}, {"false", false}, {"maybe", false}} {
		t.Setenv(key, tt.value)
		if got := envBool(key); got != tt.want {
			t.Errorf("envBool(%q) = %v, want %v", tt.value, got, tt.want)
		}
	}
}
