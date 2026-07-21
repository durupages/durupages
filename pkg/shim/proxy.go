// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package shim

import (
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/durupages/durupages/pkg/api"
	"github.com/durupages/durupages/pkg/usage"
	"github.com/durupages/durupages/pkg/workerauth"
)

// eventHeaderAllow is the small allowlist of request headers recorded in usage
// events (in addition to the always-redacted sensitive names).
var eventHeaderAllow = map[string]bool{
	"content-type": true,
	"accept":       true,
	"user-agent":   true,
	"referer":      true,
}

// serveProxy is the :8080 request handler. It verifies the lease, lazy-loads the
// target deployment, forwards to the current runtime instance and emits a
// RequestUsage event.
func (s *Shim) serveProxy(w http.ResponseWriter, r *http.Request) {
	if s.isDraining() {
		http.Error(w, "draining", http.StatusServiceUnavailable)
		return
	}

	leaseTok := r.Header.Get(api.HeaderLease)
	if leaseTok == "" {
		http.Error(w, "missing lease", http.StatusUnauthorized)
		return
	}
	claims, err := workerauth.VerifyLease(s.opts.LeasePubKey, leaseTok)
	if err != nil {
		http.Error(w, "invalid lease", http.StatusForbidden)
		return
	}
	if claims.TenantID != s.opts.TenantID {
		http.Error(w, "tenant mismatch", http.StatusForbidden)
		return
	}
	if hp := r.Header.Get(api.HeaderPage); hp != claims.PageID {
		http.Error(w, "page mismatch", http.StatusForbidden)
		return
	}

	pageID := claims.PageID
	deploymentID := r.Header.Get(api.HeaderDeployment)
	if deploymentID == "" {
		http.Error(w, "missing deployment", http.StatusBadRequest)
		return
	}

	if err := s.ensureLoaded(r.Context(), pageID, deploymentID); err != nil {
		http.Error(w, "load failed", http.StatusBadGateway)
		return
	}
	li := s.current.Load()
	if li == nil {
		http.Error(w, "no instance", http.StatusServiceUnavailable)
		return
	}

	requestID := claims.RequestID
	s.cor.expect(requestID)

	atomic.AddInt64(&li.inFlight, 1)
	defer atomic.AddInt64(&li.inFlight, -1)

	start := s.now()
	status := s.forward(w, r, li.inst.Endpoint(), pageID, requestID)
	wall := s.now().Sub(start)

	s.touch(deploymentID)
	s.recordUsage(r, claims, deploymentID, status, start, wall, requestID)
}

// forward proxies r to the runtime instance at endpoint, injecting the trusted
// page and request-id headers and stripping the lease. It returns the upstream
// status (or 502 on transport failure).
func (s *Shim) forward(w http.ResponseWriter, r *http.Request, endpoint, pageID, requestID string) int {
	out := r.Clone(r.Context())
	out.URL.Scheme = "http"
	out.URL.Host = endpoint
	out.RequestURI = ""
	out.Header.Set(api.HeaderPage, pageID)
	out.Header.Del(api.HeaderLease)
	if requestID != "" {
		out.Header.Set(api.HeaderRequestID, requestID)
	} else {
		out.Header.Del(api.HeaderRequestID)
	}

	resp, err := s.transport.RoundTrip(out)
	if err != nil {
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return http.StatusBadGateway
	}
	defer resp.Body.Close()

	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	if requestID != "" {
		w.Header().Set(api.HeaderRequestID, requestID)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
	return resp.StatusCode
}

// recordUsage builds, redacts and emits the RequestUsage event, merging the
// correlated tail trace (cpu time, logs, exceptions) when it arrives in time.
func (s *Shim) recordUsage(r *http.Request, claims *workerauth.LeaseClaims, deploymentID string, status int, start time.Time, wall time.Duration, requestID string) {
	u := usage.RequestUsage{
		RequestID:    requestID,
		TenantID:     s.opts.TenantID,
		PageID:       claims.PageID,
		DeploymentID: deploymentID,
		WorkerPod:    s.opts.PodName,
		Timestamp:    start,
		WallTime:     wall,
		Event: usage.Event{
			Request: usage.RequestInfo{
				URL:     originalURL(r),
				Method:  r.Method,
				Headers: eventHeaders(r),
			},
			Response: usage.ResponseInfo{Status: status},
		},
	}

	if trace := s.cor.wait(r.Context(), requestID, traceCorrelationWait); trace != nil {
		u.CPUTime = trace.CPUTime
		u.Logs = trace.Logs
		u.Exceptions = trace.Exceptions
	}

	if rd := s.redactor.Load(); rd != nil {
		rd.apply(&u)
	}
	s.emitter.emit(u)
}

// originalURL reconstructs the client-facing URL of the request.
func originalURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host + r.URL.RequestURI()
}

// eventHeaders captures the allowlisted plus always-redacted request headers.
func eventHeaders(r *http.Request) map[string]string {
	h := map[string]string{}
	if r.Host != "" {
		h["Host"] = r.Host
	}
	for name := range r.Header {
		lower := strings.ToLower(name)
		if eventHeaderAllow[lower] || redactHeaderNames[lower] {
			h[name] = r.Header.Get(name)
		}
	}
	return h
}
