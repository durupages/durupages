/**
 * Copyright 2026 JC-Lab
 * SPDX-License-Identifier: EPL-2.0
 */

// Single-file worker fixture for the DuruPages e2e suite.
//
// The router proxies only paths matched by _routes.json (/api/*) to the worker;
// everything else is served statically by the router. Within the worker:
//   - GET /api/hello       -> JSON handler (exercises the full dynamic path)
//   - any other /api/*     -> delegated to the static assets via env.ASSETS
export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    if (url.pathname === "/api/hello") {
      const body = {
        message: "hello from worker",
        version: "v1",
        method: request.method,
      };
      return new Response(JSON.stringify(body), {
        status: 200,
        headers: { "content-type": "application/json" },
      });
    }
    // Other worker-matched paths fall through to the static asset service.
    return env.ASSETS.fetch(request);
  },
};
