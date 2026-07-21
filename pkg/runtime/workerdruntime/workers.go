// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package workerdruntime

// entryWorkerJS is the generated, trusted entry dispatcher. It reads the
// trusted X-DuruPages-Page header (set by the shim after lease verification),
// looks up the matching service binding via the ROUTES JSON map and forwards
// the request. Unknown pages get a 404.
const entryWorkerJS = `export default {
  async fetch(request, env) {
    const page = request.headers.get("X-DuruPages-Page");
    if (!page) {
      return new Response("missing page header", { status: 404 });
    }
    // A workerd "json =" binding exposes the PARSED value (an object), not a
    // JSON string — never JSON.parse it. Accept a string defensively anyway.
    let routes = env.ROUTES;
    if (typeof routes === "string") {
      try { routes = JSON.parse(routes); } catch (e) { routes = null; }
    }
    if (!routes || typeof routes !== "object") {
      return new Response("bad routes binding", { status: 500 });
    }
    const binding = routes[page];
    const svc = binding ? env[binding] : undefined;
    if (!svc) {
      return new Response("unknown page", { status: 404 });
    }
    return svc.fetch(request);
  }
};
`

// tailWorkerJS is the generated, trusted tail worker attached to every page
// worker. It flattens each TraceItem into a JSON record and POSTs the batch to
// the shim's tail collector (env.TAIL_ENDPOINT). Its global outbound is bound
// to an external service so the loopback POST is permitted. Delivery is
// best-effort and never throws.
const tailWorkerJS = `export default {
  async tail(events, env) {
    try {
      const payload = events.map(toRecord);
      await fetch(env.TAIL_ENDPOINT, {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify(payload)
      });
    } catch (e) {
      // best-effort: swallow delivery errors.
    }
  }
};

function toRecord(item) {
  const rec = {
    scriptName: item.scriptName || "",
    outcome: item.outcome || "",
    cpuTime: item.cpuTime || 0,
    wallTime: item.wallTime || 0,
    logs: [],
    exceptions: []
  };
  for (const log of item.logs || []) {
    rec.logs.push({
      timestamp: log.timestamp || 0,
      level: log.level || "log",
      message: messageToString(log.message)
    });
  }
  for (const exc of item.exceptions || []) {
    rec.exceptions.push({
      timestamp: exc.timestamp || 0,
      name: exc.name || "",
      message: exc.message || "",
      stack: exc.stack || ""
    });
  }
  const ev = item.event;
  if (ev && ev.request) {
    rec.event = {
      request: {
        url: ev.request.url || "",
        method: ev.request.method || "",
        headers: headersToObject(ev.request.headers)
      },
      response: ev.response ? { status: ev.response.status || 0 } : null
    };
  } else {
    rec.event = null;
  }
  return rec;
}

function headersToObject(h) {
  const out = {};
  if (!h) return out;
  if (typeof h.forEach === "function") {
    h.forEach((value, key) => { out[key] = value; });
  } else {
    for (const key of Object.keys(h)) { out[key] = h[key]; }
  }
  return out;
}

function messageToString(m) {
  if (typeof m === "string") return m;
  try { return JSON.stringify(m); } catch (e) { return String(m); }
}
`
