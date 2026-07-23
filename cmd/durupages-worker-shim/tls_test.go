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
)

// caPEM returns a throwaway CA certificate in PEM form. The tests only need
// something tlsconf will accept into a pool.
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

// Nothing set means plaintext: TLS is opt-in, so an existing deployment that
// knows none of these variables must keep working.
func TestClientTLSDisabled(t *testing.T) {
	t.Setenv(envCAPEM, string(caPEM(t)))
	cfg, err := clientTLS("controller.ns.svc:9440", false, envControllerServerName)
	if err != nil {
		t.Fatal(err)
	}
	if cfg != nil {
		t.Fatal("a disabled target must stay plaintext even with a CA available")
	}
}

// The server name is what a certificate is checked against, so getting the
// derivation wrong turns every handshake into a name mismatch.
func TestClientTLSDerivesServerName(t *testing.T) {
	t.Setenv(envCAPEM, string(caPEM(t)))
	for _, tt := range []struct{ target, want string }{
		{"controller.durupages.svc.cluster.local:9440", "controller.durupages.svc.cluster.local"},
		{"https://hub.durupages.svc:9080", "hub.durupages.svc"},
		{"https://hub.example.com/v1/x", "hub.example.com"},
		{"10.0.0.5:9440", "10.0.0.5"},
	} {
		cfg, err := clientTLS(tt.target, true, envControllerServerName)
		if err != nil {
			t.Fatalf("%s: %v", tt.target, err)
		}
		if cfg.ServerName != tt.want {
			t.Errorf("clientTLS(%q).ServerName = %q, want %q", tt.target, cfg.ServerName, tt.want)
		}
	}
}

// The override exists because a pod may reach a Service by cluster IP, which no
// certificate names.
func TestClientTLSServerNameOverride(t *testing.T) {
	t.Setenv(envCAPEM, string(caPEM(t)))
	t.Setenv(envControllerServerName, "controller.durupages.svc")
	cfg, err := clientTLS("10.96.0.7:9440", true, envControllerServerName)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ServerName != "controller.durupages.svc" {
		t.Errorf("ServerName = %q, want the override", cfg.ServerName)
	}
}

// A file-based CA is verified against a hostname this code has to supply;
// tlsconf refuses to build a config without one. This is the regression that
// would break every file-CA deployment.
func TestClientTLSFileCAGetsDerivedServerName(t *testing.T) {
	file := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(file, caPEM(t), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envCAFile, file)
	cfg, err := clientTLS("hub.durupages.svc:9080", true, envHubServerName)
	if err != nil {
		t.Fatalf("file CA rejected: %v", err)
	}
	if cfg.ServerName != "hub.durupages.svc" {
		t.Errorf("ServerName = %q, want the derived name", cfg.ServerName)
	}
	if cfg.VerifyPeerCertificate == nil {
		t.Error("the file CA path should verify the peer itself (reloadable pool)")
	}
}

// Inline PEM wins: the controller injects it, and a stale file mounted
// alongside must not take precedence over what the controller just said.
func TestClientTLSInlinePEMBeatsFile(t *testing.T) {
	t.Setenv(envCAPEM, string(caPEM(t)))
	t.Setenv(envCAFile, filepath.Join(t.TempDir(), "does-not-exist.crt"))
	cfg, err := clientTLS("hub.durupages.svc:9080", true, envHubServerName)
	if err != nil {
		t.Fatalf("the unreadable file was consulted despite inline PEM: %v", err)
	}
	if cfg.RootCAs == nil {
		t.Error("inline PEM should have produced a root pool")
	}
}

func TestClientTLSRejectsGarbageCA(t *testing.T) {
	t.Setenv(envCAPEM, "not a certificate")
	if _, err := clientTLS("hub:9080", true, envHubServerName); err == nil {
		t.Fatal("garbage CA PEM was accepted, which would silently fall back to the system roots")
	}
}

func TestClientTLSSkipVerify(t *testing.T) {
	t.Setenv(envInsecureSkipVerify, "true")
	cfg, err := clientTLS("hub:9080", true, envHubServerName)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.InsecureSkipVerify {
		t.Fatal("the escape hatch did not take effect")
	}
}

// The hub bundle hop has no switch of its own; the scheme decides.
func TestHubTLSEnabled(t *testing.T) {
	for _, tt := range []struct {
		addr string
		want bool
	}{
		{"https://hub.durupages.svc:9080", true},
		{"HTTPS://hub.durupages.svc:9080", true},
		{"http://hub.durupages.svc:9080", false},
		{"hub.durupages.svc:9080", false},
		{"", false},
	} {
		if got := hubTLSEnabled(tt.addr); got != tt.want {
			t.Errorf("hubTLSEnabled(%q) = %v, want %v", tt.addr, got, tt.want)
		}
	}
}

// A typo must not be read as "true"; an unset variable must not be an error.
func TestEnvBool(t *testing.T) {
	const key = "DURUPAGES_TEST_BOOL"
	for _, tt := range []struct {
		value string
		want  bool
	}{{"", false}, {"true", true}, {"1", true}, {"TRUE", true}, {"false", false}, {"yes", false}, {"maybe", false}} {
		t.Setenv(key, tt.value)
		if got := envBool(key); got != tt.want {
			t.Errorf("envBool(%q) = %v, want %v", tt.value, got, tt.want)
		}
	}
}
