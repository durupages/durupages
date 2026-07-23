// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package controller

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/durupages/durupages/pkg/provider/memprovider"
)

// writeCA writes a self-signed CA certificate PEM to path with the given
// common name, and gives it a distinct modification time so a rewrite is
// detectable regardless of filesystem timestamp granularity.
func writeCA(t *testing.T, path, commonName string, age time.Duration) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write CA: %v", err)
	}
	mod := time.Now().Add(-age)
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
}

// caSubject returns the common name of the first certificate in a PEM bundle,
// used to tell one generated CA from another.
func caSubject(t *testing.T, pemData string) string {
	t.Helper()
	block, _ := pem.Decode([]byte(pemData))
	if block == nil {
		t.Fatalf("no PEM block in %q", pemData)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	return cert.Subject.CommonName
}

// TestBuildPodSpecTLSEnv checks the TLS settings a worker pod is born with.
func TestBuildPodSpecTLSEnv(t *testing.T) {
	caPath := filepath.Join(t.TempDir(), "ca.crt")
	writeCA(t, caPath, "ca-one", 2*time.Second)

	e := setup(t, func(o *Options) {
		o.ControllerAddr = "controller.durupages.svc:9440"
		o.HubAddr = "https://hub.durupages.svc:9460"
		o.HubLogAddr = "hub.durupages.svc:9470"
		o.WorkerCACertFile = caPath
		o.ControllerTLS = true
		o.HubLogTLS = true
		o.ControllerServerName = "controller.internal"
		o.HubServerName = "hub.internal"
		o.HubLogServerName = "hub-log.internal"
	})

	spec, err := e.c.buildPodSpec(nil, "t1", "dpw-t1-aaaaaa")
	if err != nil {
		t.Fatalf("buildPodSpec: %v", err)
	}
	if got := caSubject(t, spec.Env["DURUPAGES_CA_CERT_PEM"]); got != "ca-one" {
		t.Fatalf("DURUPAGES_CA_CERT_PEM = CA %q, want ca-one", got)
	}
	for key, want := range map[string]string{
		"DURUPAGES_CONTROLLER_TLS":         "true",
		"DURUPAGES_HUB_LOG_TLS":            "true",
		"DURUPAGES_CONTROLLER_SERVER_NAME": "controller.internal",
		"DURUPAGES_HUB_SERVER_NAME":        "hub.internal",
		"DURUPAGES_HUB_LOG_SERVER_NAME":    "hub-log.internal",
		// The addresses keep their worker-facing names: only the controller's
		// own flags gained the ADVERTISE prefix.
		"DURUPAGES_CONTROLLER_ADDR": "controller.durupages.svc:9440",
		"DURUPAGES_HUB_ADDR":        "https://hub.durupages.svc:9460",
		"DURUPAGES_HUB_LOG_ADDR":    "hub.durupages.svc:9470",
	} {
		if got := spec.Env[key]; got != want {
			t.Errorf("env %s = %q, want %q", key, got, want)
		}
	}
}

// TestBuildPodSpecTLSEnvOmitted checks that a plaintext deployment gets no TLS
// environment at all, rather than empty or "false" variables.
func TestBuildPodSpecTLSEnvOmitted(t *testing.T) {
	e := setup(t, func(o *Options) {
		o.ControllerAddr = "controller:9440"
		o.HubAddr = "http://hub:9460"
	})
	spec, err := e.c.buildPodSpec(nil, "t1", "dpw-t1-aaaaaa")
	if err != nil {
		t.Fatalf("buildPodSpec: %v", err)
	}
	for _, key := range []string{
		"DURUPAGES_CA_CERT_PEM", "DURUPAGES_CONTROLLER_TLS", "DURUPAGES_HUB_LOG_TLS",
		"DURUPAGES_CONTROLLER_SERVER_NAME", "DURUPAGES_HUB_SERVER_NAME",
		"DURUPAGES_HUB_LOG_SERVER_NAME", "DURUPAGES_HUB_LOG_ADDR",
	} {
		if v, ok := spec.Env[key]; ok {
			t.Errorf("env %s present (%q), want absent", key, v)
		}
	}
}

// TestBuildPodSpecTLSEnvSkipsHubLogWithoutAddr checks that hub-log TLS settings
// are dropped when log ingest itself is off: a worker in pod-log mode never
// dials that endpoint.
func TestBuildPodSpecTLSEnvSkipsHubLogWithoutAddr(t *testing.T) {
	e := setup(t, func(o *Options) {
		o.HubLogTLS = true
		o.HubLogServerName = "hub-log.internal"
	})
	spec, err := e.c.buildPodSpec(nil, "t1", "dpw-t1-aaaaaa")
	if err != nil {
		t.Fatalf("buildPodSpec: %v", err)
	}
	for _, key := range []string{"DURUPAGES_HUB_LOG_TLS", "DURUPAGES_HUB_LOG_SERVER_NAME"} {
		if v, ok := spec.Env[key]; ok {
			t.Errorf("env %s present (%q) without a hub log address", key, v)
		}
	}
}

// TestWorkerCARotation is the reason the CA is read per pod creation: after the
// bundle is rewritten on disk, the next pod must be created with the new one.
func TestWorkerCARotation(t *testing.T) {
	caPath := filepath.Join(t.TempDir(), "ca.crt")
	writeCA(t, caPath, "ca-old", 2*time.Second)

	e := setup(t, func(o *Options) { o.WorkerCACertFile = caPath })

	first, err := e.c.buildPodSpec(nil, "t1", "pod-1")
	if err != nil {
		t.Fatalf("buildPodSpec: %v", err)
	}
	if got := caSubject(t, first.Env["DURUPAGES_CA_CERT_PEM"]); got != "ca-old" {
		t.Fatalf("first pod got CA %q, want ca-old", got)
	}

	writeCA(t, caPath, "ca-new", 0)

	second, err := e.c.buildPodSpec(nil, "t1", "pod-2")
	if err != nil {
		t.Fatalf("buildPodSpec after rotation: %v", err)
	}
	if got := caSubject(t, second.Env["DURUPAGES_CA_CERT_PEM"]); got != "ca-new" {
		t.Fatalf("pod created after rotation got CA %q, want ca-new", got)
	}
}

// TestWorkerCAKeepsLastGood checks that a CA file that disappears (or is
// half-written) does not stop pod creation once a good bundle has been read.
func TestWorkerCAKeepsLastGood(t *testing.T) {
	caPath := filepath.Join(t.TempDir(), "ca.crt")
	writeCA(t, caPath, "ca-one", 2*time.Second)
	e := setup(t, func(o *Options) { o.WorkerCACertFile = caPath })

	if _, err := e.c.buildPodSpec(nil, "t1", "pod-1"); err != nil {
		t.Fatalf("buildPodSpec: %v", err)
	}
	if err := os.WriteFile(caPath, []byte("not a certificate"), 0o600); err != nil {
		t.Fatalf("truncate CA: %v", err)
	}
	mod := time.Now()
	if err := os.Chtimes(caPath, mod, mod); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	spec, err := e.c.buildPodSpec(nil, "t1", "pod-2")
	if err != nil {
		t.Fatalf("buildPodSpec with unreadable CA: %v", err)
	}
	if got := caSubject(t, spec.Env["DURUPAGES_CA_CERT_PEM"]); got != "ca-one" {
		t.Fatalf("got CA %q, want the last good ca-one", got)
	}
}

// TestNewRejectsBadWorkerCA checks that a missing or malformed CA path fails
// startup instead of every later scale-up.
func TestNewRejectsBadWorkerCA(t *testing.T) {
	dir := t.TempDir()
	junk := filepath.Join(dir, "junk.crt")
	if err := os.WriteFile(junk, []byte("nope"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	for name, path := range map[string]string{
		"missing":   filepath.Join(dir, "absent.crt"),
		"malformed": junk,
	} {
		t.Run(name, func(t *testing.T) {
			_, priv, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatalf("genkey: %v", err)
			}
			_, err = New(Options{
				Provider:         memprovider.New(memprovider.Options{}),
				Pods:             newFakePodManager(),
				SigningKey:       priv,
				WorkerCACertFile: path,
			})
			if err == nil {
				t.Fatal("New accepted an unusable worker CA file")
			}
			if !strings.Contains(err.Error(), "worker CA") && !strings.Contains(err.Error(), "no CA certificate") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
