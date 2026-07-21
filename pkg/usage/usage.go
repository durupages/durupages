// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

// Package usage defines the request-level usage/log event schemas. The JSON
// serialization of these types is also the pod-log (JSON Lines) format, so
// the hub path and the pod-log path share one schema.
package usage

import "time"

// RequestUsage is emitted by the worker shim for every dynamic request.
type RequestUsage struct {
	// RequestID is assigned at lease issuance (also exposed as a response
	// header for correlation).
	RequestID    string        `json:"requestId"`
	TenantID     string        `json:"tenantId"`
	PageID       string        `json:"pageId"`
	DeploymentID string        `json:"deploymentId"`
	WorkerPod    string        `json:"workerPod"`
	Timestamp    time.Time     `json:"timestamp"`
	WallTime     time.Duration `json:"wallTimeNs"`
	// CPUTime is measured by durupages-workerd (V8 level); 0 when running on
	// a stock workerd binary which hardcodes trace cpuTime to 0.
	CPUTime    time.Duration `json:"cpuTimeNs"`
	Event      Event         `json:"event"`
	Logs       []LogEntry    `json:"logs,omitempty"`
	Exceptions []Exception   `json:"exceptions,omitempty"`
	// DroppedEvents counts locally dropped events since the last successful
	// flush (buffer overflow while the hub was unreachable).
	DroppedEvents int64 `json:"droppedEvents,omitempty"`
}

// StaticAccess is emitted by the router for static asset responses.
type StaticAccess struct {
	TenantID     string    `json:"tenantId"`
	PageID       string    `json:"pageId"`
	DeploymentID string    `json:"deploymentId"`
	Timestamp    time.Time `json:"timestamp"`
	Event        Event     `json:"event"`
	BytesSent    int64     `json:"bytesSent"`
}

// Event carries request/response metadata. Header values are redacted by the
// shim/router before the event leaves the pod (sensitive header names and
// page Secret values).
type Event struct {
	Request  RequestInfo  `json:"request"`
	Response ResponseInfo `json:"response"`
}

type RequestInfo struct {
	URL     string            `json:"url"`
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers,omitempty"`
}

type ResponseInfo struct {
	Status int `json:"status"`
}

// LogEntry mirrors workerd's TraceLog {timestamp, level, message}.
type LogEntry struct {
	Timestamp time.Time `json:"timestamp"`
	// Level is one of "debug" | "info" | "log" | "warn" | "error".
	Level   string `json:"level"`
	Message string `json:"message"`
}

// Exception mirrors workerd's TraceException.
type Exception struct {
	Timestamp time.Time `json:"timestamp"`
	Name      string    `json:"name"`
	Message   string    `json:"message"`
	Stack     string    `json:"stack,omitempty"`
}
