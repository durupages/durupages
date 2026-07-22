// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package main

import (
	"strings"
	"testing"
)

// noEnv is an environment with nothing set.
func noEnv(string) string { return "" }

// envMap turns a map into a getenv.
func envMap(m map[string]string) getenv {
	return func(k string) string { return m[k] }
}

// requiredArgs are the flags every configuration needs, so a test can add just
// the ones it cares about.
func requiredArgs(extra ...string) []string {
	return append([]string{
		"--pg-dsn=postgres://localhost/durupages",
		"--worker-jwt-signing-key=/etc/durupages/signing.pem",
		"--controller-addr=controller:9440",
		"--hub-advertise-addr=http://hub:9460",
		"--worker-image=ghcr.io/durupages/worker:latest",
	}, extra...)
}

// TestParseConfigAdvertiseFlags covers the renamed advertise flags and the TLS
// facts propagated to workers.
func TestParseConfigAdvertiseFlags(t *testing.T) {
	cfg, err := parseConfig(requiredArgs(
		"--hub-advertise-addr=https://hub.internal:9460",
		"--hub-log-advertise-addr=hub-log.internal:9470",
		"--advertise-hub-log-tls",
		"--advertise-controller-server-name=controller.internal",
		"--advertise-hub-server-name=hub.internal",
		"--advertise-hub-log-server-name=hub-log.internal",
		"--worker-ca-cert-file=/etc/durupages/ca.crt",
	), noEnv)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.hubAdvertiseAddr != "https://hub.internal:9460" {
		t.Errorf("hubAdvertiseAddr = %q", cfg.hubAdvertiseAddr)
	}
	if cfg.hubLogAdvertiseAddr != "hub-log.internal:9470" {
		t.Errorf("hubLogAdvertiseAddr = %q", cfg.hubLogAdvertiseAddr)
	}
	if !cfg.advertiseHubLogTLS {
		t.Error("advertiseHubLogTLS = false, want true")
	}
	if cfg.workerCACertFile != "/etc/durupages/ca.crt" {
		t.Errorf("workerCACertFile = %q", cfg.workerCACertFile)
	}
	if cfg.controllerAdvertiseSNI != "controller.internal" ||
		cfg.hubAdvertiseSNI != "hub.internal" || cfg.hubLogAdvertiseSNI != "hub-log.internal" {
		t.Errorf("server name overrides not resolved: %+v", cfg)
	}
}

// TestParseConfigAdvertiseEnv checks the environment spelling of the renamed
// inputs.
func TestParseConfigAdvertiseEnv(t *testing.T) {
	cfg, err := parseConfig(nil, envMap(map[string]string{
		"DURUPAGES_PG_DSN":                    "postgres://localhost/durupages",
		"DURUPAGES_WORKER_JWT_SIGNING_KEY":    "/etc/durupages/signing.pem",
		"DURUPAGES_CONTROLLER_ADVERTISE_ADDR": "controller:9440",
		"DURUPAGES_HUB_ADVERTISE_ADDR":        "https://hub:9460",
		"DURUPAGES_HUB_LOG_ADVERTISE_ADDR":    "hub:9470",
		"DURUPAGES_WORKER_IMAGE":              "worker:latest",
		"DURUPAGES_HUB_LOG_TLS":               "true",
		"DURUPAGES_WORKER_CA_CERT_FILE":       "/etc/durupages/ca.crt",
	}))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.hubAdvertiseAddr != "https://hub:9460" || cfg.hubLogAdvertiseAddr != "hub:9470" {
		t.Errorf("advertise addresses not read from env: %+v", cfg)
	}
	if !cfg.advertiseHubLogTLS || cfg.workerCACertFile != "/etc/durupages/ca.crt" {
		t.Errorf("TLS advertisement not read from env: %+v", cfg)
	}
}

// TestParseConfigWorkerAnnotationsFile covers both spellings of the
// cluster-wide worker annotation source, and its "disabled" default.
func TestParseConfigWorkerAnnotationsFile(t *testing.T) {
	cfg, err := parseConfig(requiredArgs(), noEnv)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.workerAnnotationsFile != "" {
		t.Fatalf("workerAnnotationsFile = %q, want empty by default", cfg.workerAnnotationsFile)
	}

	const path = "/etc/durupages/worker-annotations/annotations.yaml"
	cfg, err = parseConfig(requiredArgs("--worker-annotations-file="+path), noEnv)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.workerAnnotationsFile != path {
		t.Fatalf("workerAnnotationsFile = %q, want %q", cfg.workerAnnotationsFile, path)
	}

	cfg, err = parseConfig(requiredArgs(), envMap(map[string]string{
		"DURUPAGES_PG_DSN":                  "postgres://localhost/durupages",
		"DURUPAGES_WORKER_ANNOTATIONS_FILE": path,
	}))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.workerAnnotationsFile != path {
		t.Fatalf("workerAnnotationsFile from env = %q, want %q", cfg.workerAnnotationsFile, path)
	}
}

// TestParseConfigRejectsRenamedEnv checks that an old variable left in a
// manifest is reported rather than ignored.
func TestParseConfigRejectsRenamedEnv(t *testing.T) {
	for _, old := range []string{"DURUPAGES_HUB_ADDR", "DURUPAGES_HUB_LOG_ADDR"} {
		_, err := parseConfig(requiredArgs(), envMap(map[string]string{old: "hub:9460"}))
		if err == nil {
			t.Fatalf("%s accepted, want an error", old)
		}
		if !strings.Contains(err.Error(), old) || !strings.Contains(err.Error(), "ADVERTISE") {
			t.Fatalf("%s: unhelpful error %v", old, err)
		}
	}
}

// TestParseConfigRejectsRenamedFlag checks the old flag spellings are gone.
func TestParseConfigRejectsRenamedFlag(t *testing.T) {
	for _, arg := range []string{"--hub-addr=hub:9460", "--hub-log-addr=hub:9470"} {
		if _, err := parseConfig(requiredArgs(arg), noEnv); err == nil {
			t.Fatalf("%s accepted, want an error", arg)
		}
	}
}

// TestParseConfigControllerTLSDefault checks that "does the controller serve
// TLS to workers" follows its own certificate unless stated otherwise.
func TestParseConfigControllerTLSDefault(t *testing.T) {
	tests := []struct {
		name string
		args []string
		env  map[string]string
		want bool
	}{
		{name: "plaintext", want: false},
		{
			name: "derived from own certificate",
			args: []string{"--tls-cert-file=/tls/tls.crt", "--tls-key-file=/tls/tls.key"},
			want: true,
		},
		{
			name: "flag overrides a TLS listener (terminating proxy in front)",
			args: []string{"--tls-cert-file=/tls/tls.crt", "--tls-key-file=/tls/tls.key",
				"--advertise-controller-tls=false"},
			want: false,
		},
		{
			name: "env overrides a plaintext listener",
			env:  map[string]string{"DURUPAGES_CONTROLLER_TLS": "true"},
			want: true,
		},
		{
			name: "flag beats env",
			env:  map[string]string{"DURUPAGES_CONTROLLER_TLS": "true"},
			args: []string{"--advertise-controller-tls=false"},
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := parseConfig(requiredArgs(tc.args...), envMap(tc.env))
			if err != nil {
				t.Fatalf("parseConfig: %v", err)
			}
			if cfg.advertiseControllerTLS != tc.want {
				t.Fatalf("advertiseControllerTLS = %v, want %v", cfg.advertiseControllerTLS, tc.want)
			}
		})
	}
}

// TestParseConfigAdminTLSInherits checks the admin API's certificate defaulting.
func TestParseConfigAdminTLSInherits(t *testing.T) {
	cfg, err := parseConfig(requiredArgs(
		"--tls-cert-file=/tls/tls.crt", "--tls-key-file=/tls/tls.key"), noEnv)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.adminTLSCertFile != "/tls/tls.crt" || cfg.adminTLSKeyFile != "/tls/tls.key" {
		t.Fatalf("admin API did not inherit the gRPC certificate: %+v", cfg)
	}

	cfg, err = parseConfig(requiredArgs(
		"--tls-cert-file=/tls/tls.crt", "--tls-key-file=/tls/tls.key",
		"--admin-tls-cert-file=/admin/tls.crt", "--admin-tls-key-file=/admin/tls.key"), noEnv)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.adminTLSCertFile != "/admin/tls.crt" || cfg.adminTLSKeyFile != "/admin/tls.key" {
		t.Fatalf("admin API override ignored: %+v", cfg)
	}
}

// TestParseConfigRejectsHalfCertificate checks a lone certificate or key is an
// error rather than a listener that quietly stays plaintext.
func TestParseConfigRejectsHalfCertificate(t *testing.T) {
	for _, args := range [][]string{
		{"--tls-cert-file=/tls/tls.crt"},
		{"--tls-key-file=/tls/tls.key"},
		{"--admin-tls-cert-file=/admin/tls.crt"},
	} {
		if _, err := parseConfig(requiredArgs(args...), noEnv); err == nil {
			t.Fatalf("%v accepted, want an error", args)
		}
	}
}

// TestParseConfigRequiredFlags checks the required set is enforced, with the
// renamed flag named in the message.
func TestParseConfigRequiredFlags(t *testing.T) {
	_, err := parseConfig([]string{
		"--pg-dsn=postgres://localhost/durupages",
		"--worker-jwt-signing-key=/etc/durupages/signing.pem",
		"--controller-addr=controller:9440",
		"--worker-image=worker:latest",
	}, noEnv)
	if err == nil {
		t.Fatal("missing --hub-advertise-addr accepted")
	}
	if !strings.Contains(err.Error(), "--hub-advertise-addr") {
		t.Fatalf("error does not name the flag: %v", err)
	}
}
