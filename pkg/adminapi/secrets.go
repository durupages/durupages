// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package adminapi

import (
	"net/http"

	"github.com/durupages/durupages/pkg/provider"
)

// maxSecretNameLen bounds a secret name. A secret becomes a worker binding, so
// the name has to stay a usable JavaScript identifier; the length cap only
// keeps a pathological name out of the database and the request line.
const maxSecretNameLen = 256

// maxBulkSecrets caps the entry count of PUT /v1/pages/{pageId}/secrets. It
// matches the limit `wrangler secret bulk` enforces, so a bundle accepted by
// one tool is accepted by the other.
const maxBulkSecrets = 100

// secretKeysResponse is the GET .../secrets body. It carries names only: the
// controller can read secret values (PageProvider.GetPage returns them, the
// worker needs them as bindings) but never hands one back to a client.
type secretKeysResponse struct {
	SecretKeys []string `json:"secretKeys"`
}

// secretValueRequest is the PUT .../secrets/{name} body.
type secretValueRequest struct {
	Value string `json:"value"`
}

// secretsBulkRequest is the PUT .../secrets body.
//
// Secrets is a pointer so that an absent (or null) field is distinguishable
// from an explicit empty object: {} clears every secret, whereas a missing
// field is rejected — clearing the whole set must be deliberate, never the
// result of a mis-spelled or forgotten key.
type secretsBulkRequest struct {
	Secrets *map[string]string `json:"secrets"`
}

// secretsPatchRequest is the PATCH .../secrets body.
//
// Unlike the PUT form the values are *string, because a null value deletes
// that one key while every secret the request does not mention is preserved.
// This mirrors `wrangler secret bulk`, where the file upserts and a null
// removes.
type secretsPatchRequest struct {
	Secrets *map[string]*string `json:"secrets"`
}

// validSecretName reports whether name is usable as a worker binding
// identifier: ^[A-Za-z_][A-Za-z0-9_]*$, at most maxSecretNameLen bytes.
//
// It is deliberately stricter than validID (no '-' or '.', which are not legal
// in an identifier) and at the same time laxer (a leading '_' is fine), so
// pathID cannot stand in for it on the {name} wildcard.
func validSecretName(name string) bool {
	if name == "" || len(name) > maxSecretNameLen {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
		case i > 0 && c >= '0' && c <= '9':
		default:
			return false
		}
	}
	return true
}

// secretName reads the {name} wildcard and validates it, answering 400 when it
// is not a binding identifier.
func secretName(w http.ResponseWriter, r *http.Request) (string, bool) {
	v := r.PathValue("name")
	if !validSecretName(v) {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "invalid secret name %q", v)
		return "", false
	}
	return v, true
}

// handleListSecrets serves GET /v1/pages/{pageId}/secrets.
//
// It reports the sorted secret names, never the values: the controller holds
// them (they are worker bindings) but the API is write-only about them, so a
// client can see which secrets exist and nothing more. This is the one secrets
// route that does not write, so it needs no AdminProvider and works on a
// read-only controller.
func (s *Server) handleListSecrets(w http.ResponseWriter, r *http.Request) {
	pageID, ok := pathID(w, r, "pageId")
	if !ok {
		return
	}
	p, err := s.provider.GetPage(r.Context(), pageID)
	if err != nil {
		writeProviderError(w, err, "page %q does not exist", pageID)
		return
	}
	keys := secretKeys(p.Config.Secret)
	if keys == nil {
		keys = []string{}
	}
	writeJSON(w, http.StatusOK, secretKeysResponse{SecretKeys: keys})
}

// handlePutSecret serves PUT /v1/pages/{pageId}/secrets/{name}: it creates the
// secret when absent and overwrites its value otherwise, leaving every other
// secret alone.
//
// Per-key management only works server-side: a client never receives the other
// values (responses carry config.secretKeys alone), so it cannot POST /v1/pages
// with a complete map. The controller can — GetPage returns the values — so it
// performs the read, changes the single key and writes the page back. The
// response is the ordinary page JSON, i.e. still key names only; the value just
// written is not echoed.
func (s *Server) handlePutSecret(w http.ResponseWriter, r *http.Request) {
	admin, ok := s.requireAdmin(w)
	if !ok {
		return
	}
	pageID, ok := pathID(w, r, "pageId")
	if !ok {
		return
	}
	name, ok := secretName(w, r)
	if !ok {
		return
	}
	var in secretValueRequest
	if err := decodeJSON(w, r, &in); err != nil {
		writeDecodeError(w, err)
		return
	}
	s.updateSecrets(w, r, admin, pageID, func(secrets map[string]string) map[string]string {
		secrets[name] = in.Value
		return secrets
	})
}

// handleDeleteSecret serves DELETE /v1/pages/{pageId}/secrets/{name}.
//
// Deletion is idempotent: removing a name that is not set answers 200 with the
// unchanged page. Like the upsert it is a server-side read-modify-write,
// because only the controller can see the values it has to preserve.
func (s *Server) handleDeleteSecret(w http.ResponseWriter, r *http.Request) {
	admin, ok := s.requireAdmin(w)
	if !ok {
		return
	}
	pageID, ok := pathID(w, r, "pageId")
	if !ok {
		return
	}
	name, ok := secretName(w, r)
	if !ok {
		return
	}
	s.updateSecrets(w, r, admin, pageID, func(secrets map[string]string) map[string]string {
		delete(secrets, name)
		return secrets
	})
}

// handleReplaceSecrets serves PUT /v1/pages/{pageId}/secrets: it replaces the
// page's whole secret map, exactly like config.secret on POST /v1/pages but
// without touching any other field of the page.
//
// An explicit empty object clears every secret; an absent or null "secrets"
// field is refused so that a typo cannot wipe the set by accident. The values
// in the request are stored and never appear in the response, which is the
// ordinary page JSON with config.secretKeys only.
func (s *Server) handleReplaceSecrets(w http.ResponseWriter, r *http.Request) {
	admin, ok := s.requireAdmin(w)
	if !ok {
		return
	}
	pageID, ok := pathID(w, r, "pageId")
	if !ok {
		return
	}
	var in secretsBulkRequest
	if err := decodeJSON(w, r, &in); err != nil {
		writeDecodeError(w, err)
		return
	}
	if in.Secrets == nil {
		writeError(w, http.StatusBadRequest, codeInvalidRequest,
			`missing "secrets" object (use {"secrets":{}} to clear every secret)`)
		return
	}
	if len(*in.Secrets) > maxBulkSecrets {
		writeError(w, http.StatusBadRequest, codeInvalidRequest,
			"too many secrets: %d, limit is %d", len(*in.Secrets), maxBulkSecrets)
		return
	}
	for k := range *in.Secrets {
		if !validSecretName(k) {
			writeError(w, http.StatusBadRequest, codeInvalidRequest, "invalid secret name %q", k)
			return
		}
	}
	replacement := *in.Secrets
	s.updateSecrets(w, r, admin, pageID, func(map[string]string) map[string]string {
		return cloneSecrets(replacement)
	})
}

// handlePatchSecrets serves PATCH /v1/pages/{pageId}/secrets: it upserts the
// named secrets and leaves every other one alone, which is what
// `wrangler secret bulk` does. A null value deletes that key (also wrangler's
// convention); deleting a key that does not exist is not an error.
//
// PUT on the same collection replaces the whole map instead. Both exist because
// "apply this file of secrets" and "these are now all my secrets" are different
// intents, and only the client knows which one it means.
//
// As with the other secrets routes the server does the read-modify-write, since
// a client never holds the current values.
func (s *Server) handlePatchSecrets(w http.ResponseWriter, r *http.Request) {
	admin, ok := s.requireAdmin(w)
	if !ok {
		return
	}
	pageID, ok := pathID(w, r, "pageId")
	if !ok {
		return
	}
	var in secretsPatchRequest
	if err := decodeJSON(w, r, &in); err != nil {
		writeDecodeError(w, err)
		return
	}
	if in.Secrets == nil {
		writeError(w, http.StatusBadRequest, codeInvalidRequest,
			`missing "secrets" object (use PUT with {"secrets":{}} to clear every secret)`)
		return
	}
	if len(*in.Secrets) > maxBulkSecrets {
		writeError(w, http.StatusBadRequest, codeInvalidRequest,
			"too many secrets: %d, limit is %d", len(*in.Secrets), maxBulkSecrets)
		return
	}
	for k := range *in.Secrets {
		if !validSecretName(k) {
			writeError(w, http.StatusBadRequest, codeInvalidRequest, "invalid secret name %q", k)
			return
		}
	}
	patch := *in.Secrets
	s.updateSecrets(w, r, admin, pageID, func(current map[string]string) map[string]string {
		for k, v := range patch {
			if v == nil {
				delete(current, k)
				continue
			}
			current[k] = *v
		}
		return current
	})
}

// updateSecrets is the read-modify-write shared by the three mutating secrets
// routes: it reads the page, hands apply a *copy* of the stored secret map
// (the map GetPage returned is never mutated in place, since a provider may
// hand out a shared value), writes the page back with the result and renders
// the stored page.
//
// Only Config.Secret changes; every other field is written back as read.
// CustomDomains is passed through untouched even though UpsertPage ignores it
// by provider contract (domains are owned by SetCustomDomains) — so the stored
// set survives, and re-reading the page keeps it in the response.
//
// The read and the write are not atomic: a concurrent secret change between
// them is lost. The controller runs as a single replica and secret edits are
// rare operator actions, so the race is accepted rather than solved with a
// provider-level compare-and-swap.
func (s *Server) updateSecrets(w http.ResponseWriter, r *http.Request, admin provider.AdminProvider,
	pageID string, apply func(secrets map[string]string) map[string]string) {
	p, err := s.provider.GetPage(r.Context(), pageID)
	if err != nil {
		writeProviderError(w, err, "page %q does not exist", pageID)
		return
	}
	updated := *p
	updated.Config.Secret = apply(cloneSecrets(p.Config.Secret))
	if len(updated.Config.Secret) == 0 {
		// Normalize "no secrets" to nil: an empty map and a nil map mean the
		// same thing to every provider and to toPageJSON.
		updated.Config.Secret = nil
	}
	if err := admin.UpsertPage(r.Context(), updated); err != nil {
		writeProviderError(w, err, "page %q does not exist", pageID)
		return
	}
	s.writePage(w, r, pageID)
}

// cloneSecrets copies a secret map, returning an empty (never nil) map so a
// caller can add a key to a page that has no secrets yet.
func cloneSecrets(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
