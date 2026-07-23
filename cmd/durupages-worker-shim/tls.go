// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package main

import (
	"crypto/tls"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/durupages/durupages/pkg/tlsconf"
)

// Client TLS is configured entirely through the environment because a worker
// pod has no command line: the controller creates it and injects these
// variables (see pkg/controller/workertls.go).
//
// Every variable is optional. With none of them set the shim dials plaintext,
// exactly as it did before TLS existed -- transport security is opt-in, per
// hop, so a cluster can be converted one component at a time.
const (
	// envCAPEM carries the CA as inline PEM. It wins over envCAFile: worker
	// pods live in their own namespace and cannot mount the controller's
	// Secret, so the environment is the normal path for them.
	envCAPEM  = "DURUPAGES_CA_CERT_PEM"
	envCAFile = "DURUPAGES_CA_CERT_FILE"

	// envControllerTLS and envHubLogTLS turn TLS on for the two gRPC hops. The
	// hub bundle hop has no flag of its own: it is HTTP, so DURUPAGES_HUB_ADDR
	// having an https:// scheme is what says "use TLS", and a second switch
	// could only contradict it.
	envControllerTLS = "DURUPAGES_CONTROLLER_TLS"
	envHubLogTLS     = "DURUPAGES_HUB_LOG_TLS"

	// Server-name overrides, for when the dial target does not appear in the
	// certificate -- reaching a Service by cluster IP, most often.
	envControllerServerName = "DURUPAGES_CONTROLLER_SERVER_NAME"
	envHubServerName        = "DURUPAGES_HUB_SERVER_NAME"
	envHubLogServerName     = "DURUPAGES_HUB_LOG_SERVER_NAME"

	// envInsecureSkipVerify is the escape hatch for bringing a cluster up
	// before its PKI is in place. It makes the traffic confidential but not
	// authenticated, so its use is always logged as a warning.
	envInsecureSkipVerify = "DURUPAGES_TLS_INSECURE_SKIP_VERIFY"
)

// envBool reads a boolean environment variable, treating anything unparsable as
// false. A typo must not silently disable verification, so the value is logged
// when it is rejected.
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
// The server name is derived from the target when no override is given. That
// derivation is not a nicety: tlsconf's file-based CA path verifies the
// hostname itself and refuses to build a config without one, and leaving it
// empty on the inline-PEM path would verify against the dial authority, which
// is an IP address as often as not.
func clientTLS(target string, enabled bool, serverNameEnv string) (*tls.Config, error) {
	if !enabled {
		return nil, nil
	}
	opts := tlsconf.ClientOptions{
		ServerName:         os.Getenv(serverNameEnv),
		InsecureSkipVerify: envBool(envInsecureSkipVerify),
	}
	if pem := os.Getenv(envCAPEM); pem != "" {
		opts.CAPEM = []byte(pem)
	} else {
		opts.CAFile = os.Getenv(envCAFile)
	}
	if opts.ServerName == "" {
		opts.ServerName = tlsconf.HostFromTarget(target)
	}
	return tlsconf.ClientConfig(opts)
}

// hubTLSEnabled reports whether bundle downloads go over TLS, which the hub
// address's scheme decides on its own.
func hubTLSEnabled(hubAddr string) bool {
	return strings.HasPrefix(strings.ToLower(hubAddr), "https://")
}

// logTLSStatus records which hops are encrypted, so that a handshake failure
// later in the log can be read against what the pod was actually asked to do.
func logTLSStatus(controller, hub, hubLog *tls.Config) {
	if envBool(envInsecureSkipVerify) {
		slog.Warn("TLS certificate verification is disabled",
			slog.String("env", envInsecureSkipVerify),
			slog.String("impact", "connections are encrypted but not authenticated; any host on the path can impersonate the server"))
	}
	attrs := []any{
		slog.Bool("controller", controller != nil),
		slog.Bool("hub", hub != nil),
		slog.Bool("hubLog", hubLog != nil),
	}
	for _, cfg := range []struct {
		name string
		cfg  *tls.Config
	}{{"controllerServerName", controller}, {"hubServerName", hub}, {"hubLogServerName", hubLog}} {
		if cfg.cfg != nil && cfg.cfg.ServerName != "" {
			attrs = append(attrs, slog.String(cfg.name, cfg.cfg.ServerName))
		}
	}
	slog.Info("client TLS configured", attrs...)
}
