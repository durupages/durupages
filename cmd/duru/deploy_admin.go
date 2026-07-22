// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// deployViaAdmin deploys through the controller's admin API. The client needs
// no database or object storage credentials: it streams a tar.gz of the build
// output and the controller does the scan, upload and activation.
func deployViaAdmin(o deployOptions, adminURL string) error {
	c := newAdminClient(adminURL, "", nil, defaultTimeout)

	// Create the tenant/page only when missing so redeploys keep existing
	// config (an upsert with an empty body would reset it).
	if err := c.ensureTenant(o.TenantID); err != nil {
		return err
	}
	if err := c.ensurePage(o.PageID, o.TenantID); err != nil {
		return err
	}

	// Secrets are written before the upload, which is what activates the new
	// deployment: a worker must never come live holding stale ones.
	if o.Secrets != nil {
		if _, err := c.setSecrets(o.PageID, *o.Secrets); err != nil {
			var se *apiStatusError
			if errors.As(err, &se) {
				return fmt.Errorf("secrets: %w", se.verbose())
			}
			return fmt.Errorf("secrets: %w", err)
		}
		o.reportSecrets(len(*o.Secrets))
	}

	res, err := c.deployUpload(o.Dir, o.PageID, o.DeploymentID)
	if err != nil {
		return err
	}
	if o.Domains != "" {
		if err := c.deploySetCustomDomains(o.PageID, strings.Split(o.Domains, ",")); err != nil {
			return fmt.Errorf("custom domains: %w", err)
		}
	}

	o.DeploymentID = res.DeploymentID
	o.report(res.Manifest.StaticFileCount, res.Manifest.HasWorker)
	return nil
}

// uploadResult is the subset of the admin API upload response the CLI reports.
// It mirrors pkg/adminapi's response shape, where the manifest summary is
// nested under "manifest".
type uploadResult struct {
	DeploymentID string `json:"deploymentId"`
	Activated    bool   `json:"activated"`
	Manifest     struct {
		HasWorker       bool `json:"hasWorker"`
		StaticFileCount int  `json:"staticFileCount"`
	} `json:"manifest"`
}

// ensureTenant creates the tenant when it does not exist yet.
func (c *adminClient) ensureTenant(tenantID string) error {
	path := "/v1/tenants/" + url.PathEscape(tenantID)
	ok, err := c.exists(path)
	if err != nil || ok {
		return err
	}
	return c.postJSON("/v1/tenants", map[string]any{"id": tenantID})
}

// ensurePage creates the page when it does not exist yet.
func (c *adminClient) ensurePage(pageID, tenantID string) error {
	path := "/v1/pages/" + url.PathEscape(pageID)
	ok, err := c.exists(path)
	if err != nil || ok {
		return err
	}
	return c.postJSON("/v1/pages", map[string]any{"id": pageID, "tenantId": tenantID})
}

// deploySetCustomDomains replaces the page's domain set, reporting failures in
// the message format `duru deploy` has always used.
func (c *adminClient) deploySetCustomDomains(pageID string, domains []string) error {
	if _, err := c.setCustomDomains(pageID, domains); err != nil {
		var se *apiStatusError
		if errors.As(err, &se) {
			return se.verbose()
		}
		return err
	}
	return nil
}

// deployUpload uploads the build output and decodes the summary `duru deploy`
// prints, reporting failures in that command's message format.
func (c *adminClient) deployUpload(dir, pageID, deploymentID string) (*uploadResult, error) {
	body, err := c.uploadDeployment(dir, pageID, deploymentID, true)
	if err != nil {
		var se *apiStatusError
		if errors.As(err, &se) {
			return nil, se.verbose()
		}
		return nil, err
	}
	var res uploadResult
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, fmt.Errorf("decode upload response: %w", err)
	}
	if res.DeploymentID == "" {
		res.DeploymentID = deploymentID
	}
	return &res, nil
}
