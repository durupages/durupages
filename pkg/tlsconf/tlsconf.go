// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

// Package tlsconf builds the TLS configuration shared by every DuruPages
// component, so that the controller, hub, router and worker shim agree on how
// certificates are loaded and how peers are verified.
//
// Transport security is opt-in: a component given no certificate serves
// plaintext, and a client told nothing about a CA dials plaintext. That keeps
// an existing plaintext deployment working untouched while letting an operator
// turn TLS on one hop at a time.
//
// # Server certificates
//
// ServerConfig reloads the key pair from disk when it changes, because
// cert-manager renews a certificate by rewriting the mounted Secret in place.
// Without that a renewed certificate would only take effect on a restart, and
// the pod would keep serving the old one until it expired.
//
// # Client trust
//
// Clients verify servers against an explicitly supplied CA (inline PEM or a
// file), falling back to the system roots when neither is given. The worker
// shim receives its CA as inline PEM: worker pods live in their own namespace
// and are created by the controller at runtime, so shipping the CA in the pod
// environment avoids replicating a Secret across namespaces. A CA certificate
// is public material, so carrying it in a pod spec discloses nothing.
//
// A CA supplied as a file is re-read on change too, for the same reason the
// server certificate is: it is a mounted Secret and can be rotated underneath a
// running process. tls.Config has no hook for swapping RootCAs, so verification
// against a reloadable pool is done in VerifyPeerCertificate, which is why a
// CAFile requires a ServerName -- the hostname check that RootCAs verification
// would have done has to be performed explicitly. Use HostFromTarget to derive
// one from a dial address.
//
// Inline CA PEM is not reloadable: it comes from the process environment, which
// cannot change without a restart. That suits worker pods, which are recreated
// rather than reconfigured.
package tlsconf

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

// ClientOptions describes how a client should verify the server it dials.
type ClientOptions struct {
	// CAPEM is inline PEM holding one or more CA certificates. Takes
	// precedence over CAFile.
	CAPEM []byte
	// CAFile is a path to PEM CA certificates.
	CAFile string
	// ServerName overrides the name verified in the server certificate. Set it
	// when connecting by IP or through an address that does not match a SAN --
	// an isolated network reaching a Service by cluster IP, for example.
	ServerName string
	// InsecureSkipVerify disables verification entirely. It exists for bringing
	// a cluster up before the PKI is in place; it makes TLS confidential but
	// not authenticated, so anything on the path can impersonate the server.
	InsecureSkipVerify bool
}

// Enabled reports whether the options ask for TLS at all.
func (o ClientOptions) Enabled() bool {
	return len(o.CAPEM) > 0 || o.CAFile != "" || o.InsecureSkipVerify
}

// HostFromTarget returns the host part of a dial target ("host:port", a bare
// host, or a URL), for use as ClientOptions.ServerName.
func HostFromTarget(target string) string {
	s := target
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?"); i >= 0 {
		s = s[:i]
	}
	if host, _, err := net.SplitHostPort(s); err == nil {
		return host
	}
	return s
}

// ClientConfig builds a *tls.Config verifying a server per opts.
//
// With CAFile set the pool is re-read when the file changes, which requires
// opts.ServerName so the hostname can still be checked; ClientConfig reports an
// error when it is missing rather than silently skipping that check.
func ClientConfig(opts ClientOptions) (*tls.Config, error) {
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		ServerName:         opts.ServerName,
		InsecureSkipVerify: opts.InsecureSkipVerify,
	}
	if opts.InsecureSkipVerify {
		return cfg, nil
	}

	if len(opts.CAPEM) > 0 {
		pool, err := poolFromPEM(opts.CAPEM)
		if err != nil {
			return nil, err
		}
		cfg.RootCAs = pool
		return cfg, nil
	}
	if opts.CAFile == "" {
		return cfg, nil // system roots
	}

	if opts.ServerName == "" {
		return nil, fmt.Errorf("tlsconf: ServerName is required with CAFile " +
			"(the reloadable-CA path verifies the hostname itself); derive one with HostFromTarget")
	}
	ca := &caReloader{file: opts.CAFile}
	if _, err := ca.pool(); err != nil {
		return nil, err
	}
	// RootCAs cannot be swapped on a live tls.Config, so verification moves
	// into VerifyPeerCertificate, which reads the current pool per handshake.
	// Disabling the built-in check is what makes room for it; the manual check
	// below is equivalent, hostname included.
	cfg.InsecureSkipVerify = true
	serverName := opts.ServerName
	cfg.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return fmt.Errorf("tlsconf: server presented no certificate")
		}
		certs := make([]*x509.Certificate, 0, len(rawCerts))
		for _, raw := range rawCerts {
			c, err := x509.ParseCertificate(raw)
			if err != nil {
				return fmt.Errorf("tlsconf: parse server certificate: %w", err)
			}
			certs = append(certs, c)
		}
		pool, err := ca.pool()
		if err != nil {
			return err
		}
		inter := x509.NewCertPool()
		for _, c := range certs[1:] {
			inter.AddCert(c)
		}
		_, err = certs[0].Verify(x509.VerifyOptions{
			Roots:         pool,
			Intermediates: inter,
			DNSName:       serverName,
			KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		})
		return err
	}
	return cfg, nil
}

// poolFromPEM parses CA certificates, rejecting PEM that yields none: a pool
// that silently stayed empty would fall back to the system roots and turn every
// internal certificate into an unrelated-looking handshake failure.
func poolFromPEM(pem []byte) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("tlsconf: no CA certificate found in the supplied PEM")
	}
	return pool, nil
}

// caReloader re-reads a CA file when it changes on disk.
type caReloader struct {
	file string

	mu       sync.RWMutex
	cur      *x509.CertPool
	mod      time.Time
	lastStat time.Time
}

func (c *caReloader) pool() (*x509.CertPool, error) {
	c.mu.RLock()
	cur, last := c.cur, c.lastStat
	c.mu.RUnlock()
	if cur != nil && time.Since(last) < statInterval {
		return cur, nil
	}

	info, err := os.Stat(c.file)
	if err != nil {
		if cur != nil {
			return cur, nil // keep trusting what we have
		}
		return nil, fmt.Errorf("tlsconf: stat CA file %q: %w", c.file, err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastStat = time.Now()
	if c.cur != nil && info.ModTime().Equal(c.mod) {
		return c.cur, nil
	}
	b, err := os.ReadFile(c.file)
	if err != nil {
		if c.cur != nil {
			return c.cur, nil
		}
		return nil, fmt.Errorf("tlsconf: read CA file %q: %w", c.file, err)
	}
	pool, err := poolFromPEM(b)
	if err != nil {
		if c.cur != nil {
			return c.cur, nil // a half-written file must not break verification
		}
		return nil, err
	}
	c.cur = pool
	c.mod = info.ModTime()
	return c.cur, nil
}

// ServerConfig builds a *tls.Config serving the key pair at certFile/keyFile.
// The pair is loaded once up front so that a bad path fails at startup, and
// re-read afterwards whenever either file's modification time changes.
func ServerConfig(certFile, keyFile string) (*tls.Config, error) {
	r := &reloader{certFile: certFile, keyFile: keyFile}
	if _, err := r.load(); err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: r.getCertificate,
	}, nil
}

// reloader holds the current key pair and swaps it when the files change.
type reloader struct {
	certFile, keyFile string

	mu       sync.RWMutex
	cert     *tls.Certificate
	certMod  time.Time
	keyMod   time.Time
	lastStat time.Time
}

// statInterval bounds how often the files are stat'ed, so a busy listener does
// not stat twice per handshake. It is a variable only so that tests can drive
// rotation without waiting.
var statInterval = 10 * time.Second

func (r *reloader) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.RLock()
	cert, last := r.cert, r.lastStat
	r.mu.RUnlock()

	if cert != nil && time.Since(last) < statInterval {
		return cert, nil
	}
	c, err := r.load()
	if err != nil {
		// Keep serving the certificate we have: a renewal that briefly leaves
		// the pair unreadable must not take the listener down.
		if cert != nil {
			return cert, nil
		}
		return nil, err
	}
	return c, nil
}

// load re-reads the pair when either file changed, and returns the current one.
func (r *reloader) load() (*tls.Certificate, error) {
	certInfo, err := os.Stat(r.certFile)
	if err != nil {
		return nil, fmt.Errorf("tlsconf: stat certificate %q: %w", r.certFile, err)
	}
	keyInfo, err := os.Stat(r.keyFile)
	if err != nil {
		return nil, fmt.Errorf("tlsconf: stat key %q: %w", r.keyFile, err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastStat = time.Now()
	if r.cert != nil && certInfo.ModTime().Equal(r.certMod) && keyInfo.ModTime().Equal(r.keyMod) {
		return r.cert, nil
	}
	pair, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return nil, fmt.Errorf("tlsconf: load key pair %q/%q: %w", r.certFile, r.keyFile, err)
	}
	r.cert = &pair
	r.certMod = certInfo.ModTime()
	r.keyMod = keyInfo.ModTime()
	return r.cert, nil
}
