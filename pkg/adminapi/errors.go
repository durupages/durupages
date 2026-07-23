// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package adminapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/durupages/durupages/pkg/provider"
)

// Error codes carried by the error envelope.
const (
	codeInvalidRequest   = "invalid_request"
	codeInvalidBundle    = "invalid_bundle"
	codeNotFound         = "not_found"
	codeTooLarge         = "payload_too_large"
	codeMethodNotAllowed = "method_not_allowed"
	codeInternal         = "internal"
	codeNotImplemented   = "not_implemented"
)

// errorEnvelope is the single error shape of the API:
//
//	{"error":{"code":"not_found","message":"..."}}
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

// errorBody carries the machine-readable code and a human-readable message.
type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeJSON encodes v as the response body with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "encode response: %v", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", fmt.Sprint(len(b)))
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// serverErrorRecorder is implemented by responseRecorder: it collects the
// detail behind a 5xx reply so that ServeHTTP can log it server-side.
type serverErrorRecorder interface {
	recordServerError(msg string)
}

// maxUnwrapDepth bounds the walk down a chain of ResponseWriter wrappers.
const maxUnwrapDepth = 8

// recordServerError attaches the cause of a 5xx reply to the request log line.
//
// It is a best-effort, defensive hook: handlers normally see the Server's
// responseRecorder, but a test may drive them with a bare
// httptest.ResponseRecorder and a middleware may wrap the writer in its own
// type, so anything that is neither the recorder nor an unwrappable wrapper
// around it is simply skipped. The response is written either way.
func recordServerError(w http.ResponseWriter, status int, msg string) {
	if status < http.StatusInternalServerError {
		return
	}
	for i := 0; i < maxUnwrapDepth; i++ {
		if rec, ok := w.(serverErrorRecorder); ok {
			rec.recordServerError(msg)
			return
		}
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return
		}
		w = u.Unwrap()
	}
}

// writeError renders the error envelope with the given status and code. For a
// 5xx it also records the message for the server-side request log, so that the
// cause of a server fault is visible to the operator and not only to the
// client that happened to make the request.
func writeError(w http.ResponseWriter, status int, code, format string, args ...any) {
	env := errorEnvelope{Error: errorBody{Code: code, Message: fmt.Sprintf(format, args...)}}
	recordServerError(w, status, env.Error.Message)
	b, err := json.Marshal(env)
	if err != nil {
		b = []byte(`{"error":{"code":"internal","message":"encode error"}}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", fmt.Sprint(len(b)))
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// writeProviderError maps a provider error onto the envelope: ErrNotFound
// becomes 404 with the caller's message, everything else 500 carrying the
// provider's own message.
//
// That message stays in the 500 body on purpose. This API is served on a
// private, operator-only port (see the package Security note) and the duru CLI
// prints the envelope message verbatim, which is how faults like a read-only
// upload directory get diagnosed at all; replacing it with a bare "internal
// error" would only move the diagnosis to the controller's log without buying
// any real confidentiality here. The same detail is now recorded server-side
// too, so it is available even when nobody kept the client output.
func writeProviderError(w http.ResponseWriter, err error, notFoundFormat string, args ...any) {
	if errors.Is(err, provider.ErrNotFound) {
		writeError(w, http.StatusNotFound, codeNotFound, notFoundFormat, args...)
		return
	}
	writeError(w, http.StatusInternalServerError, codeInternal, "%v", err)
}

// writeNotImplemented reports that the configured provider or storage cannot
// serve this route.
func writeNotImplemented(w http.ResponseWriter, what string) {
	writeError(w, http.StatusNotImplemented, codeNotImplemented,
		"%s is not configured on this controller", what)
}

// requireAdmin returns the AdminProvider, answering 501 when none is wired.
func (s *Server) requireAdmin(w http.ResponseWriter) (provider.AdminProvider, bool) {
	if s.admin == nil {
		writeNotImplemented(w, "the admin (write) provider")
		return nil, false
	}
	return s.admin, true
}

// decodeJSON reads a JSON request body into v. Unknown fields are rejected so
// that typos in an admin request fail loudly instead of being ignored.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	// Reject trailing content so that two concatenated documents are an error.
	if err := dec.Decode(new(json.RawMessage)); err == nil {
		return errors.New("unexpected trailing data after JSON document")
	}
	return nil
}

// writeDecodeError maps a body-decoding failure onto 413 (too large) or 400.
func writeDecodeError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, codeTooLarge,
			"request body exceeds %d bytes", tooLarge.Limit)
		return
	}
	writeError(w, http.StatusBadRequest, codeInvalidRequest, "invalid JSON body: %v", err)
}

// maxIDLen bounds identifiers so they stay usable as storage key segments.
const maxIDLen = 128

// validID reports whether s is safe as a tenant, page or deployment
// identifier: it must be non-empty, at most maxIDLen bytes, start with an
// alphanumeric and otherwise consist of alphanumerics, '-', '_' and '.'. This
// keeps identifiers safe in filesystem paths, storage keys and URLs.
func validID(s string) bool {
	if s == "" || len(s) > maxIDLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case i > 0 && (c == '-' || c == '_' || c == '.'):
		default:
			return false
		}
	}
	return true
}

// pathID reads a path wildcard and validates it, answering 400 when malformed.
func pathID(w http.ResponseWriter, r *http.Request, name string) (string, bool) {
	v := r.PathValue(name)
	if !validID(v) {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "invalid %s %q", name, v)
		return "", false
	}
	return v, true
}
