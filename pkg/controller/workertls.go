// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package controller

import (
	"crypto/x509"
	"fmt"
	"os"
	"sync"
	"time"
)

// caFileCache serves the CA bundle that worker pods receive as
// DURUPAGES_CA_CERT_PEM, re-reading the file whenever it changes on disk.
//
// Reading per pod creation rather than once at startup is what makes CA
// rotation work: the file is a mounted Secret that cert-manager rewrites in
// place, and a bundle cached for the controller's lifetime would hand every pod
// created after a rotation a CA that no longer signs the certificates it is
// about to verify. Pod creation is rare enough that stat'ing on every call
// costs nothing, so freshness is bounded by the filesystem rather than by a
// polling interval.
type caFileCache struct {
	file string

	mu   sync.Mutex
	pem  []byte
	mod  time.Time
	size int64
}

// newCAFileCache reads the bundle once so that a missing or malformed CA file
// is reported at startup instead of on the first scale-up.
func newCAFileCache(file string) (*caFileCache, error) {
	c := &caFileCache{file: file}
	if _, err := c.bytes(); err != nil {
		return nil, err
	}
	return c, nil
}

// bytes returns the current CA PEM.
//
// Once a good bundle has been read, a later failure (a half-written file during
// rotation, a transient stat error) keeps serving the last good one: the
// previous CA is far more likely to be usable than no CA at all, and a pod
// created without one cannot reach the controller at all. Only the very first
// read can fail, and that failure fails controller startup.
func (c *caFileCache) bytes() ([]byte, error) {
	info, err := os.Stat(c.file)

	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		if c.pem != nil {
			return c.pem, nil
		}
		return nil, fmt.Errorf("controller: stat worker CA file %q: %w", c.file, err)
	}
	if c.pem != nil && info.ModTime().Equal(c.mod) && info.Size() == c.size {
		return c.pem, nil
	}

	b, err := os.ReadFile(c.file)
	if err != nil {
		if c.pem != nil {
			return c.pem, nil
		}
		return nil, fmt.Errorf("controller: read worker CA file %q: %w", c.file, err)
	}
	// Parse before adopting: PEM that yields no certificate would leave workers
	// falling back to the system roots, which turns an internal certificate
	// into a confusing "unknown authority" at the first request instead of a
	// clear controller-side error here.
	if !x509.NewCertPool().AppendCertsFromPEM(b) {
		if c.pem != nil {
			return c.pem, nil
		}
		return nil, fmt.Errorf("controller: no CA certificate found in %q", c.file)
	}

	c.pem, c.mod, c.size = b, info.ModTime(), info.Size()
	return c.pem, nil
}

// workerTLSEnv adds the TLS settings a worker pod needs to verify the endpoints
// it dials, into env.
//
// The CA travels as inline PEM rather than as a mounted Secret because worker
// pods live in their own namespace and are created at runtime; a CA
// certificate is public material, so carrying it in the pod spec discloses
// nothing. The cost is that the value is fixed at pod creation: an already
// running pod keeps the CA it was born with, and only pods created after a
// rotation see the new one. That is unavoidable for env-carried material —
// during a CA overlap window the bundle should hold both the old and the new
// CA so existing pods keep verifying successfully.
//
// Whether an endpoint speaks TLS is knowledge the controller has and the worker
// does not, which is why it is stated here rather than probed. Only "true" is
// emitted; an absent variable means plaintext.
func (c *Controller) workerTLSEnv(env map[string]string) error {
	if c.workerCA != nil {
		pem, err := c.workerCA.bytes()
		if err != nil {
			return err
		}
		env["DURUPAGES_CA_CERT_PEM"] = string(pem)
	}
	if c.opts.ControllerTLS {
		env["DURUPAGES_CONTROLLER_TLS"] = "true"
	}
	// Only meaningful alongside the address itself: without HubLogAddr the
	// worker stays in pod-log mode and never dials the hub's log service.
	if c.opts.HubLogTLS && c.opts.HubLogAddr != "" {
		env["DURUPAGES_HUB_LOG_TLS"] = "true"
	}
	setIfNotEmpty(env, "DURUPAGES_CONTROLLER_SERVER_NAME", c.opts.ControllerServerName)
	setIfNotEmpty(env, "DURUPAGES_HUB_SERVER_NAME", c.opts.HubServerName)
	if c.opts.HubLogAddr != "" {
		setIfNotEmpty(env, "DURUPAGES_HUB_LOG_SERVER_NAME", c.opts.HubLogServerName)
	}
	return nil
}

// setIfNotEmpty keeps empty values out of the pod environment, where they would
// read as "configured to the empty string" rather than "not configured".
func setIfNotEmpty(env map[string]string, key, value string) {
	if value != "" {
		env[key] = value
	}
}
