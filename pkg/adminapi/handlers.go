// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package adminapi

import (
	"errors"
	"net/http"

	"github.com/durupages/durupages/pkg/provider"
)

// handleListTenants serves GET /v1/tenants.
func (s *Server) handleListTenants(w http.ResponseWriter, r *http.Request) {
	admin, ok := s.requireAdmin(w)
	if !ok {
		return
	}
	tenants, err := admin.ListTenants(r.Context())
	if err != nil {
		writeProviderError(w, err, "no tenants")
		return
	}
	out := tenantsResponse{Tenants: make([]tenantJSON, 0, len(tenants))}
	for _, t := range tenants {
		out.Tenants = append(out.Tenants, toTenantJSON(t))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleUpsertTenant serves POST /v1/tenants: it creates the tenant when
// absent and replaces its configuration otherwise.
func (s *Server) handleUpsertTenant(w http.ResponseWriter, r *http.Request) {
	admin, ok := s.requireAdmin(w)
	if !ok {
		return
	}
	var in tenantJSON
	if err := decodeJSON(w, r, &in); err != nil {
		writeDecodeError(w, err)
		return
	}
	if !validID(in.ID) {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "invalid tenant id %q", in.ID)
		return
	}
	t := in.toProvider()
	if err := admin.UpsertTenant(r.Context(), t); err != nil {
		writeProviderError(w, err, "tenant %q does not exist", t.ID)
		return
	}
	writeJSON(w, http.StatusOK, toTenantJSON(t))
}

// handleGetTenant serves GET /v1/tenants/{tenantId}.
func (s *Server) handleGetTenant(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := pathID(w, r, "tenantId")
	if !ok {
		return
	}
	t, err := s.provider.GetTenant(r.Context(), tenantID)
	if err != nil {
		writeProviderError(w, err, "tenant %q does not exist", tenantID)
		return
	}
	writeJSON(w, http.StatusOK, toTenantJSON(*t))
}

// handleDeleteTenant serves DELETE /v1/tenants/{tenantId}. Deletion is
// idempotent: removing an unknown tenant still answers 204.
func (s *Server) handleDeleteTenant(w http.ResponseWriter, r *http.Request) {
	admin, ok := s.requireAdmin(w)
	if !ok {
		return
	}
	tenantID, ok := pathID(w, r, "tenantId")
	if !ok {
		return
	}
	if err := admin.DeleteTenant(r.Context(), tenantID); err != nil {
		writeProviderError(w, err, "tenant %q does not exist", tenantID)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleListTenantPages serves GET /v1/tenants/{tenantId}/pages.
func (s *Server) handleListTenantPages(w http.ResponseWriter, r *http.Request) {
	admin, ok := s.requireAdmin(w)
	if !ok {
		return
	}
	tenantID, ok := pathID(w, r, "tenantId")
	if !ok {
		return
	}
	if _, err := s.provider.GetTenant(r.Context(), tenantID); err != nil {
		writeProviderError(w, err, "tenant %q does not exist", tenantID)
		return
	}
	s.writePages(w, r, admin, tenantID)
}

// handleListPages serves GET /v1/pages (every tenant).
func (s *Server) handleListPages(w http.ResponseWriter, r *http.Request) {
	admin, ok := s.requireAdmin(w)
	if !ok {
		return
	}
	s.writePages(w, r, admin, "")
}

// writePages lists and renders the pages of tenantID ("" means all tenants).
func (s *Server) writePages(w http.ResponseWriter, r *http.Request, admin provider.AdminProvider, tenantID string) {
	pages, err := admin.ListPages(r.Context(), tenantID)
	if err != nil {
		writeProviderError(w, err, "tenant %q does not exist", tenantID)
		return
	}
	out := pagesResponse{Pages: make([]pageJSON, 0, len(pages))}
	for _, p := range pages {
		out.Pages = append(out.Pages, toPageJSON(p))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleUpsertPage serves POST /v1/pages: it creates the page when absent and
// updates it otherwise.
//
// Two fields are merged rather than replaced when the request omits them, so
// that a partial update cannot silently destroy state that has no other
// source: config.secret (absent keeps the stored secrets, an explicit object
// replaces them) and activeDeploymentId (absent keeps the active deployment,
// which is otherwise only moved by the activate route).
//
// customDomains needs a second call: AdminProvider.UpsertPage deliberately
// ignores the field (domains are owned by SetCustomDomains), so an explicitly
// supplied list is applied separately — otherwise it would be accepted and
// silently dropped. Omitting the field leaves the stored set untouched.
func (s *Server) handleUpsertPage(w http.ResponseWriter, r *http.Request) {
	admin, ok := s.requireAdmin(w)
	if !ok {
		return
	}
	var in pageJSON
	if err := decodeJSON(w, r, &in); err != nil {
		writeDecodeError(w, err)
		return
	}
	if !validID(in.ID) {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "invalid page id %q", in.ID)
		return
	}
	if !validID(in.TenantID) {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "invalid tenantId %q", in.TenantID)
		return
	}

	p := in.toProvider()
	existing, err := s.provider.GetPage(r.Context(), p.ID)
	switch {
	case err == nil:
		if in.Config.Secret == nil {
			p.Config.Secret = existing.Config.Secret
		}
		if in.CustomDomains == nil {
			p.CustomDomains = existing.CustomDomains
		}
		if p.ActiveDeploymentID == "" {
			p.ActiveDeploymentID = existing.ActiveDeploymentID
		}
	case errors.Is(err, provider.ErrNotFound):
		// New page: nothing to merge.
	default:
		writeError(w, http.StatusInternalServerError, codeInternal, "%v", err)
		return
	}

	if err := admin.UpsertPage(r.Context(), p); err != nil {
		writeProviderError(w, err, "tenant %q does not exist", p.TenantID)
		return
	}
	if in.CustomDomains != nil {
		if err := admin.SetCustomDomains(r.Context(), p.ID, *in.CustomDomains); err != nil {
			writeProviderError(w, err, "page %q does not exist", p.ID)
			return
		}
		// Re-read so the response reflects the normalization the provider
		// applies (lowercase, de-duplicated, sorted).
		if stored, err := s.provider.GetPage(r.Context(), p.ID); err == nil {
			p.CustomDomains = stored.CustomDomains
		}
	}
	writeJSON(w, http.StatusOK, toPageJSON(p))
}

// handleGetPage serves GET /v1/pages/{pageId}.
func (s *Server) handleGetPage(w http.ResponseWriter, r *http.Request) {
	pageID, ok := pathID(w, r, "pageId")
	if !ok {
		return
	}
	p, err := s.provider.GetPage(r.Context(), pageID)
	if err != nil {
		writeProviderError(w, err, "page %q does not exist", pageID)
		return
	}
	writeJSON(w, http.StatusOK, toPageJSON(*p))
}

// handleDeletePage serves DELETE /v1/pages/{pageId}. Deletion is idempotent.
func (s *Server) handleDeletePage(w http.ResponseWriter, r *http.Request) {
	admin, ok := s.requireAdmin(w)
	if !ok {
		return
	}
	pageID, ok := pathID(w, r, "pageId")
	if !ok {
		return
	}
	if err := admin.DeletePage(r.Context(), pageID); err != nil {
		writeProviderError(w, err, "page %q does not exist", pageID)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleSetCustomDomains serves PUT /v1/pages/{pageId}/custom-domains: it
// replaces the page's whole domain set.
func (s *Server) handleSetCustomDomains(w http.ResponseWriter, r *http.Request) {
	admin, ok := s.requireAdmin(w)
	if !ok {
		return
	}
	pageID, ok := pathID(w, r, "pageId")
	if !ok {
		return
	}
	var in customDomainsRequest
	if err := decodeJSON(w, r, &in); err != nil {
		writeDecodeError(w, err)
		return
	}
	for _, d := range in.Domains {
		if d == "" {
			writeError(w, http.StatusBadRequest, codeInvalidRequest, "empty custom domain")
			return
		}
	}
	if _, err := s.provider.GetPage(r.Context(), pageID); err != nil {
		writeProviderError(w, err, "page %q does not exist", pageID)
		return
	}
	if err := admin.SetCustomDomains(r.Context(), pageID, in.Domains); err != nil {
		writeProviderError(w, err, "page %q does not exist", pageID)
		return
	}
	s.writePage(w, r, pageID)
}

// handleListDeployments serves GET /v1/pages/{pageId}/deployments.
func (s *Server) handleListDeployments(w http.ResponseWriter, r *http.Request) {
	admin, ok := s.requireAdmin(w)
	if !ok {
		return
	}
	pageID, ok := pathID(w, r, "pageId")
	if !ok {
		return
	}
	page, err := s.provider.GetPage(r.Context(), pageID)
	if err != nil {
		writeProviderError(w, err, "page %q does not exist", pageID)
		return
	}
	deployments, err := admin.ListDeployments(r.Context(), pageID)
	if err != nil {
		writeProviderError(w, err, "page %q does not exist", pageID)
		return
	}
	out := deploymentsResponse{Deployments: make([]deploymentJSON, 0, len(deployments))}
	for _, d := range deployments {
		out.Deployments = append(out.Deployments, toDeploymentJSON(d, page.ActiveDeploymentID))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleActivateDeployment serves
// POST /v1/pages/{pageId}/deployments/{deploymentId}/activate.
func (s *Server) handleActivateDeployment(w http.ResponseWriter, r *http.Request) {
	admin, ok := s.requireAdmin(w)
	if !ok {
		return
	}
	pageID, ok := pathID(w, r, "pageId")
	if !ok {
		return
	}
	deploymentID, ok := pathID(w, r, "deploymentId")
	if !ok {
		return
	}
	if _, err := s.provider.GetPage(r.Context(), pageID); err != nil {
		writeProviderError(w, err, "page %q does not exist", pageID)
		return
	}
	deployments, err := admin.ListDeployments(r.Context(), pageID)
	if err != nil {
		writeProviderError(w, err, "page %q does not exist", pageID)
		return
	}
	found := false
	for _, d := range deployments {
		if d.ID == deploymentID {
			found = true
			break
		}
	}
	if !found {
		writeError(w, http.StatusNotFound, codeNotFound,
			"deployment %q does not exist on page %q", deploymentID, pageID)
		return
	}
	if err := admin.SetActiveDeployment(r.Context(), pageID, deploymentID); err != nil {
		writeProviderError(w, err, "page %q does not exist", pageID)
		return
	}
	s.writePage(w, r, pageID)
}

// writePage re-reads and renders a page after a mutation.
func (s *Server) writePage(w http.ResponseWriter, r *http.Request, pageID string) {
	p, err := s.provider.GetPage(r.Context(), pageID)
	if err != nil {
		writeProviderError(w, err, "page %q does not exist", pageID)
		return
	}
	writeJSON(w, http.StatusOK, toPageJSON(*p))
}
