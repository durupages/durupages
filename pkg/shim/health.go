// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package shim

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
)

// healthResponse is the /healthz body the controller reconcile loop reads.
type healthResponse struct {
	TenantID    string            `json:"tenantId"`
	State       string            `json:"state"`
	InFlight    int64             `json:"inFlight"`
	LoadedPages map[string]string `json:"loadedPages"`
}

// healthMux builds the :9090 health server: /healthz, /readyz and /drain.
func (s *Shim) healthMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/readyz", s.handleReadyz)
	mux.HandleFunc("/drain", s.handleDrain)
	return mux
}

func (s *Shim) handleHealthz(w http.ResponseWriter, r *http.Request) {
	inFlight := int64(0)
	if li := s.current.Load(); li != nil {
		inFlight = atomic.LoadInt64(&li.inFlight)
	}
	s.mu.Lock()
	loaded := make(map[string]string, len(s.active))
	for pageID, depID := range s.active {
		loaded[pageID] = depID
	}
	s.mu.Unlock()

	resp := healthResponse{
		TenantID:    s.opts.TenantID,
		State:       s.state(inFlight),
		InFlight:    inFlight,
		LoadedPages: loaded,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleReadyz reports readiness. The pod is ready as soon as the shim is
// serving: it boots bundle-less and lazy-loads on first request.
func (s *Shim) handleReadyz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// handleDrain lets the controller mark the pod draining (POST) or read the
// current drain state (GET).
func (s *Shim) handleDrain(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		s.setDraining(true)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{"draining": s.isDraining()})
}
