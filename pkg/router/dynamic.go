// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package router

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/durupages/durupages/pkg/api"
	"github.com/durupages/durupages/pkg/manifest"
	"google.golang.org/grpc/status"
)

// serveDynamic handles a worker-routed request (docs/ARCHITECTURE.md 5.3): it
// acquires a slot from the controller queue, proxies the original request to
// the leased worker endpoint with the internal routing/lease headers, and
// releases the slot afterwards. _redirects/_headers are NOT applied to worker
// responses (Cloudflare-compatible).
//
// This is the path with the most ways to fail that the client cannot see: the
// controller may be unreachable, the queue may time out, the leased endpoint
// may be unroutable, or the worker may itself answer 5xx. Each of those writes
// the same terse body to the client and the distinguishing detail to the
// operational log.
func (rt *Router) serveDynamic(w http.ResponseWriter, r *http.Request, rl *reqLog, page *api.PageInfo, _ *manifest.Manifest) {
	// Derive a cancelable context so returning from this handler (or the client
	// disconnecting) closes the AcquireSlot stream and cancels the queue wait.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	stream, err := rt.resolver.AcquireSlot(ctx, &api.AcquireSlotRequest{
		TenantId: page.GetTenantId(),
		PageId:   page.GetPageId(),
	})
	if err != nil {
		rt.logEvent(r, rl, slog.LevelError, msgAcquireFailed,
			slog.Int("status", http.StatusBadGateway),
			slog.String("grpcCode", status.Code(err).String()),
			slog.Any("error", err))
		http.Error(w, "502 bad gateway", http.StatusBadGateway)
		return
	}

	var lease *api.Lease
	var queuePos int64
	for lease == nil {
		ev, err := stream.Recv()
		if err != nil {
			// The client went away, or the stream ended without a grant.
			if ctx.Err() != nil {
				return
			}
			rt.logEvent(r, rl, slog.LevelError, msgAcquireStream,
				slog.Int("status", http.StatusBadGateway),
				slog.String("grpcCode", status.Code(err).String()),
				slog.Int64("queuePosition", queuePos),
				slog.Any("error", err))
			http.Error(w, "502 bad gateway", http.StatusBadGateway)
			return
		}
		switch e := ev.Event.(type) {
		case *api.AcquireSlotEvent_Queued_:
			// Still waiting in the queue; keep receiving.
			queuePos = e.Queued.GetPosition()
		case *api.AcquireSlotEvent_Timeout_:
			// Capacity, not a fault: the page hit its concurrency limit and the
			// queue wait expired. Warn so it can be alerted on separately from
			// the 502s above.
			rt.logEvent(r, rl, slog.LevelWarn, msgQueueTimeout,
				slog.Int("status", http.StatusTooManyRequests),
				slog.Int64("queuePosition", queuePos))
			w.Header().Set("Retry-After", "1")
			http.Error(w, "429 too many requests", http.StatusTooManyRequests)
			return
		case *api.AcquireSlotEvent_Granted:
			lease = e.Granted
		}
	}

	// From here on every line carries the lease's request ID, which is also the
	// X-DuruPages-Request-Id echoed to the client and forwarded to the worker:
	// one identifier correlates the client's failed response, this log and the
	// shim's log for the same request.
	rl.requestID = lease.GetRequestId()

	rt.proxyToWorker(w, r, rl, lease)

	// Release the slot on a fresh context so a client disconnect does not skip
	// the release RPC.
	if _, err := rt.resolver.ReleaseSlot(context.Background(), &api.ReleaseSlotRequest{LeaseId: lease.GetLeaseId()}); err != nil {
		// The response is already written, so this cannot change the reply —
		// but a release that never lands leaks a slot, which shows up later as
		// unexplained queue timeouts. Record it.
		rt.logEvent(r, rl, slog.LevelWarn, msgReleaseFailed,
			slog.String("leaseId", lease.GetLeaseId()),
			slog.String("grpcCode", status.Code(err).String()),
			slog.Any("error", err))
	}
}

// proxyToWorker reverse-proxies r to the leased worker endpoint, injecting the
// internal routing/lease headers and echoing the request ID on the response.
//
// The lease signature is a bearer credential for the worker and is never
// logged; the endpoint is logged in redacted form (any URL userinfo stripped).
func (rt *Router) proxyToWorker(w http.ResponseWriter, r *http.Request, rl *reqLog, lease *api.Lease) {
	target, err := url.Parse(lease.GetEndpoint())
	if err != nil {
		// Typically a scheme-less "host:port" endpoint: the failure is in the
		// lease, not in the worker, so log the endpoint we were handed.
		rt.logEvent(r, rl, slog.LevelError, msgBadEndpoint,
			slog.Int("status", http.StatusBadGateway),
			slog.String("endpoint", lease.GetEndpoint()),
			slog.String("leaseId", lease.GetLeaseId()),
			slog.Any("error", err))
		http.Error(w, "502 bad gateway", http.StatusBadGateway)
		return
	}
	endpoint := target.Redacted()
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
		if resp.StatusCode >= http.StatusInternalServerError {
			// The router reached the worker and the worker failed. Logging it
			// here is what separates "the data plane is broken" from "this
			// deployment's worker is broken" — without this line a shim that
			// answers 502 "load failed" is indistinguishable from a router that
			// could not proxy at all.
			rt.logEvent(r, rl, slog.LevelError, msgUpstream5xx,
				slog.Int("status", resp.StatusCode),
				slog.Int("upstreamStatus", resp.StatusCode),
				slog.String("endpoint", endpoint),
				slog.String("leaseId", lease.GetLeaseId()))
		}
		return nil
	}
	proxy.ErrorHandler = func(rw http.ResponseWriter, _ *http.Request, err error) {
		rt.logEvent(r, rl, slog.LevelError, msgProxyFailed,
			slog.Int("status", http.StatusBadGateway),
			slog.String("endpoint", endpoint),
			slog.String("leaseId", lease.GetLeaseId()),
			slog.Any("error", err))
		http.Error(rw, "502 bad gateway", http.StatusBadGateway)
	}
	proxy.ServeHTTP(w, r)
}
