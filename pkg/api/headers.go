// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package api

// Internal HTTP header names shared by router, worker shim and the entry
// dispatcher worker. Externally supplied values for these headers MUST be
// stripped by the router before proxying.
const (
	// HeaderPage selects the target page worker inside a tenant worker pod.
	HeaderPage = "X-DuruPages-Page"
	// HeaderDeployment is the page's active deployment at lease issuance;
	// the shim lazy-loads / hot-updates based on it.
	HeaderDeployment = "X-DuruPages-Deployment"
	// HeaderLease carries the signed lease token (see workerauth.IssueLease);
	// the shim only forwards requests bearing a valid lease.
	HeaderLease = "X-DuruPages-Lease"
	// HeaderRequestID correlates proxied requests, runtime traces and usage
	// events. Also echoed on responses.
	HeaderRequestID = "X-DuruPages-Request-Id"
)
