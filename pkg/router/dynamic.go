// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package router

import (
	"context"
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/durupages/durupages/pkg/api"
	"github.com/durupages/durupages/pkg/manifest"
)

// serveDynamic handles a worker-routed request (docs/ARCHITECTURE.md 5.3): it
// acquires a slot from the controller queue, proxies the original request to
// the leased worker endpoint with the internal routing/lease headers, and
// releases the slot afterwards. _redirects/_headers are NOT applied to worker
// responses (Cloudflare-compatible).
func (rt *Router) serveDynamic(w http.ResponseWriter, r *http.Request, page *api.PageInfo, _ *manifest.Manifest) {
	// Derive a cancelable context so returning from this handler (or the client
	// disconnecting) closes the AcquireSlot stream and cancels the queue wait.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	stream, err := rt.resolver.AcquireSlot(ctx, &api.AcquireSlotRequest{
		TenantId: page.GetTenantId(),
		PageId:   page.GetPageId(),
	})
	if err != nil {
		http.Error(w, "502 bad gateway", http.StatusBadGateway)
		return
	}

	var lease *api.Lease
	for lease == nil {
		ev, err := stream.Recv()
		if err != nil {
			// The client went away, or the stream ended without a grant.
			if ctx.Err() != nil {
				return
			}
			http.Error(w, "502 bad gateway", http.StatusBadGateway)
			return
		}
		switch e := ev.Event.(type) {
		case *api.AcquireSlotEvent_Queued_:
			// Still waiting in the queue; keep receiving.
		case *api.AcquireSlotEvent_Timeout_:
			w.Header().Set("Retry-After", "1")
			http.Error(w, "429 too many requests", http.StatusTooManyRequests)
			return
		case *api.AcquireSlotEvent_Granted:
			lease = e.Granted
		}
	}

	rt.proxyToWorker(w, r, lease)

	// Release the slot on a fresh context so a client disconnect does not skip
	// the release RPC.
	_, _ = rt.resolver.ReleaseSlot(context.Background(), &api.ReleaseSlotRequest{LeaseId: lease.GetLeaseId()})
}

// proxyToWorker reverse-proxies r to the leased worker endpoint, injecting the
// internal routing/lease headers and echoing the request ID on the response.
func (rt *Router) proxyToWorker(w http.ResponseWriter, r *http.Request, lease *api.Lease) {
	target, err := url.Parse(lease.GetEndpoint())
	if err != nil {
		http.Error(w, "502 bad gateway", http.StatusBadGateway)
		return
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	origDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		origDirector(req)
		req.Header.Set(api.HeaderPage, lease.GetPageId())
		req.Header.Set(api.HeaderDeployment, lease.GetDeploymentId())
		req.Header.Set(api.HeaderLease, lease.GetSignature())
		req.Header.Set(api.HeaderRequestID, lease.GetRequestId())
	}
	proxy.ModifyResponse = func(resp *http.Response) error {
		resp.Header.Set(api.HeaderRequestID, lease.GetRequestId())
		return nil
	}
	proxy.ErrorHandler = func(rw http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(rw, "502 bad gateway", http.StatusBadGateway)
	}
	proxy.ServeHTTP(w, r)
}
