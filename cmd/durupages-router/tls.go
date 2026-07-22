// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package main

import (
	"crypto/tls"
	"log/slog"
	"os"
	"strconv"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/durupages/durupages/pkg/tlsconf"
)

// grpcCreds turns a (possibly nil) TLS config into gRPC transport credentials;
// nil means plaintext.
func grpcCreds(cfg *tls.Config) credentials.TransportCredentials {
	if cfg == nil {
		return insecure.NewCredentials()
	}
	return credentials.NewTLS(cfg)
}

// tlsFlags holds the client-side TLS settings shared by the router's two gRPC
// hops. Like every other router setting these are flags with environment
// defaults; the environment names match the ones the controller injects into
// worker pods, so one deployment can configure the whole data plane the same
// way.
//
// All of it is optional: with nothing set the router dials plaintext, exactly
// as it did before TLS existed.
type tlsFlags struct {
	caPEM      *string
	caFile     *string
	skipVerify *bool
}

// envBool reads a boolean environment variable, treating anything unparsable as
// false. Logging the rejected value matters more here than elsewhere: a typo in
// the skip-verify switch would otherwise change verification silently.
func envBool(key string) bool {
	v := os.Getenv(key)
	if v == "" {
		return false
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		slog.Warn("invalid boolean environment variable, treating as false",
			slog.String("name", key), slog.String("value", v))
		return false
	}
	return b
}

// clientTLS builds the TLS config for one dial target, or nil when that hop
// stays plaintext.
//
// serverName falls back to the host part of the target. That derivation is not
// a nicety: tlsconf's file-based CA path verifies the hostname itself and
// refuses to build a config without one, and an empty name on the inline-PEM
// path would verify against the dial authority, which is an IP address as often
// as not.
func (f tlsFlags) clientTLS(target string, enabled bool, serverName string) (*tls.Config, error) {
	if !enabled {
		return nil, nil
	}
	opts := tlsconf.ClientOptions{
		ServerName:         serverName,
		InsecureSkipVerify: *f.skipVerify,
	}
	// Inline PEM wins over the file, matching the worker shim: a component
	// given both was most likely handed the inline copy on purpose.
	if *f.caPEM != "" {
		opts.CAPEM = []byte(*f.caPEM)
	} else {
		opts.CAFile = *f.caFile
	}
	if opts.ServerName == "" {
		opts.ServerName = tlsconf.HostFromTarget(target)
	}
	return tlsconf.ClientConfig(opts)
}

// logTLSStatus records which hops are encrypted, so a handshake failure further
// down the log can be read against what the router was actually asked to do.
func (f tlsFlags) logTLSStatus(controller, hubLog *tls.Config) {
	if *f.skipVerify {
		slog.Warn("TLS certificate verification is disabled",
			slog.String("flag", "--tls-insecure-skip-verify"),
			slog.String("impact", "connections are encrypted but not authenticated; any host on the path can impersonate the server"))
	}
	attrs := []any{
		slog.Bool("controller", controller != nil),
		slog.Bool("hubLog", hubLog != nil),
	}
	if controller != nil && controller.ServerName != "" {
		attrs = append(attrs, slog.String("controllerServerName", controller.ServerName))
	}
	if hubLog != nil && hubLog.ServerName != "" {
		attrs = append(attrs, slog.String("hubLogServerName", hubLog.ServerName))
	}
	slog.Info("client TLS configured", attrs...)
}
