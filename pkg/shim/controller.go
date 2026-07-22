// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package shim

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/metadata"

	"github.com/durupages/durupages/pkg/api"
)

// authCtx attaches the worker JWT as gRPC "authorization" metadata; the
// controller's WorkerService requires it on every call.
func (s *Shim) authCtx(ctx context.Context) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+s.currentJWT())
}

// runController registers the pod with the controller and maintains the
// heartbeat stream. If heartbeats fail continuously for heartbeatFailWindow the
// shim self-terminates so no orphan pod lingers when the controller is gone.
func (s *Shim) runController(ctx context.Context) {
	client, closeConn, err := s.workerClient()
	if err != nil {
		// Without a controller the pod cannot receive drain/renewal signals;
		// self-terminate so it does not run unmanaged.
		s.log().Error(logMsgControllerDialFailed,
			slog.String("controllerAddr", s.opts.ControllerAddr),
			slog.Bool("tls", s.opts.ControllerTLS != nil),
			slog.String("error", err.Error()))
		s.selfTerminate()
		return
	}
	defer closeConn()

	s.register(ctx, client)

	var firstFail time.Time
	for ctx.Err() == nil {
		sentOK, err := s.heartbeatSession(ctx, client)
		if sentOK {
			firstFail = time.Time{}
		}
		if err == nil {
			return // ctx cancelled: clean stop
		}
		if firstFail.IsZero() {
			firstFail = s.now()
		}
		s.log().Warn(logMsgHeartbeatFailed,
			slog.String("controllerAddr", s.opts.ControllerAddr),
			slog.Bool("tls", s.opts.ControllerTLS != nil),
			slog.Duration("failingFor", s.now().Sub(firstFail)),
			slog.String("error", err.Error()))
		if s.now().Sub(firstFail) >= heartbeatFailWindow {
			// The pod is about to remove itself. Say so, with the reason: from
			// the outside this looks like a pod that vanished on its own.
			s.log().Error(logMsgSelfTerminate,
				slog.String("controllerAddr", s.opts.ControllerAddr),
				slog.Bool("tls", s.opts.ControllerTLS != nil),
				slog.Duration("failingFor", s.now().Sub(firstFail)),
				slog.String("error", err.Error()))
			s.selfTerminate()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

// register performs the one-shot Register RPC, retrying briefly. Registration
// failures are non-fatal: the heartbeat loop keeps trying.
func (s *Shim) register(ctx context.Context, client api.WorkerServiceClient) {
	req := &api.RegisterRequest{
		TenantId: s.opts.TenantID,
		PodName:  s.opts.PodName,
		Endpoint: s.ProxyAddr(),
	}
	for attempt := 0; attempt < 3 && ctx.Err() == nil; attempt++ {
		_, err := client.Register(s.authCtx(ctx), req)
		if err == nil {
			return
		}
		// The last attempt is the one that decides this pod's fate, so it is
		// the one worth an error. Earlier ones are warnings: a controller that
		// is still starting is a normal race, not a fault.
		level := slog.LevelWarn
		if attempt == 2 {
			level = slog.LevelError
		}
		s.log().LogAttrs(ctx, level, logMsgRegisterFailed,
			slog.String("controllerAddr", s.opts.ControllerAddr),
			slog.Bool("tls", s.opts.ControllerTLS != nil),
			slog.Int("attempt", attempt+1),
			slog.String("error", err.Error()))
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

// heartbeatSession runs one heartbeat stream until it errors or ctx is
// cancelled. It reports whether at least one heartbeat was sent (used to reset
// the failure window) and the terminal error (nil on clean ctx cancellation).
func (s *Shim) heartbeatSession(ctx context.Context, client api.WorkerServiceClient) (sentOK bool, err error) {
	// Stream metadata is fixed at stream start; a renewed JWT takes effect on
	// the next heartbeat session.
	stream, err := client.Heartbeat(s.authCtx(ctx))
	if err != nil {
		return false, err
	}

	recvErr := make(chan error, 1)
	go func() {
		for {
			resp, rerr := stream.Recv()
			if rerr != nil {
				recvErr <- rerr
				return
			}
			s.applyHeartbeatResponse(resp)
		}
	}()

	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	// Send the first heartbeat immediately.
	if err := stream.Send(s.heartbeatRequest()); err != nil {
		return false, err
	}
	sentOK = true

	for {
		select {
		case <-ctx.Done():
			_ = stream.CloseSend()
			return sentOK, nil
		case rerr := <-recvErr:
			return sentOK, rerr
		case <-ticker.C:
			if err := stream.Send(s.heartbeatRequest()); err != nil {
				return sentOK, err
			}
		}
	}
}

// heartbeatRequest snapshots the current pod state for a heartbeat.
func (s *Shim) heartbeatRequest() *api.HeartbeatRequest {
	inFlight := int64(0)
	if li := s.current.Load(); li != nil {
		inFlight = atomic.LoadInt64(&li.inFlight)
	}

	s.mu.Lock()
	loaded := make([]*api.LoadedPage, 0, len(s.active))
	for pageID, depID := range s.active {
		loaded = append(loaded, &api.LoadedPage{PageId: pageID, DeploymentId: depID})
	}
	s.mu.Unlock()

	return &api.HeartbeatRequest{
		State:       s.state(inFlight),
		InFlight:    inFlight,
		LoadedPages: loaded,
	}
}

// applyHeartbeatResponse honors a drain instruction and JWT renewal.
func (s *Shim) applyHeartbeatResponse(resp *api.HeartbeatResponse) {
	if resp == nil {
		return
	}
	if resp.GetDrain() {
		s.setDraining(true)
	}
	if jwt := resp.GetRenewedJwt(); jwt != "" {
		s.stateMu.Lock()
		s.workerJWT = jwt
		s.stateMu.Unlock()
	}
}

// state returns the reported lifecycle state string.
func (s *Shim) state(inFlight int64) string {
	if s.isDraining() {
		return "draining"
	}
	if inFlight > 0 {
		return "serving"
	}
	if li := s.current.Load(); li != nil {
		return "idle"
	}
	return "ready"
}

func (s *Shim) setDraining(v bool) {
	s.stateMu.Lock()
	s.draining = v
	s.stateMu.Unlock()
}

func (s *Shim) isDraining() bool {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.draining
}

func (s *Shim) currentJWT() string {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.workerJWT
}
