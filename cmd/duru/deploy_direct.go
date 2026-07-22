// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/durupages/durupages/pkg/bundle"
	"github.com/durupages/durupages/pkg/provider"
	"github.com/durupages/durupages/pkg/provider/postgres"
	"github.com/durupages/durupages/pkg/storage/s3"
)

// directOptions configures the direct deploy mode, which writes to Storage and
// the PageProvider from the client machine. It needs database and object
// storage credentials; admin mode (deploy_admin.go) needs neither.
type directOptions struct {
	PGDSN   string
	Migrate bool

	S3Endpoint  string
	S3Region    string
	S3Bucket    string
	S3AccessKey string
	S3SecretKey string
	S3PathStyle bool
}

// deployDirect scans the build output, uploads it to Storage, registers the
// tenant/page/deployment and switches the active deployment.
func deployDirect(o deployOptions, d directOptions) error {
	ctx := context.Background()

	store, err := s3.New(ctx, s3.Options{Endpoint: d.S3Endpoint, Region: d.S3Region, Bucket: d.S3Bucket,
		AccessKey: d.S3AccessKey, SecretKey: d.S3SecretKey, UsePathStyle: d.S3PathStyle})
	if err != nil {
		return fmt.Errorf("s3: %w", err)
	}
	prov, err := postgres.New(ctx, postgres.Options{DSN: d.PGDSN, PagesDomain: o.PagesDomain})
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	defer prov.Close()
	if d.Migrate {
		if err := prov.Migrate(ctx); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}

	m, err := bundle.Deploy(ctx, store, o.Dir, bundle.ScanOptions{
		TenantID: o.TenantID, PageID: o.PageID, DeploymentID: o.DeploymentID,
	})
	if err != nil {
		return err
	}

	// Create tenant/page only when missing so redeploys keep existing config.
	if _, err := prov.GetTenant(ctx, o.TenantID); errors.Is(err, provider.ErrNotFound) {
		if err := prov.UpsertTenant(ctx, provider.Tenant{ID: o.TenantID}); err != nil {
			return fmt.Errorf("upsert tenant: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("get tenant: %w", err)
	}
	if _, err := prov.GetPage(ctx, o.PageID); errors.Is(err, provider.ErrNotFound) {
		if err := prov.UpsertPage(ctx, provider.Page{ID: o.PageID, TenantID: o.TenantID}); err != nil {
			return fmt.Errorf("upsert page: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("get page: %w", err)
	}

	// Secrets are replaced before the deployment is activated, so a worker
	// never comes live holding stale ones. Every other page field is preserved
	// by writing back the page that was just read.
	if o.Secrets != nil {
		page, err := prov.GetPage(ctx, o.PageID)
		if err != nil {
			return fmt.Errorf("get page: %w", err)
		}
		page.Config.Secret = *o.Secrets
		if err := prov.UpsertPage(ctx, *page); err != nil {
			return fmt.Errorf("set secrets: %w", err)
		}
		o.reportSecrets(len(*o.Secrets))
	}

	if err := prov.CreateDeployment(ctx, provider.Deployment{ID: o.DeploymentID, PageID: o.PageID, CreatedAt: time.Now()}); err != nil {
		return fmt.Errorf("create deployment: %w", err)
	}
	if err := prov.SetActiveDeployment(ctx, o.PageID, o.DeploymentID); err != nil {
		return fmt.Errorf("activate: %w", err)
	}
	if o.Domains != "" {
		if err := prov.SetCustomDomains(ctx, o.PageID, strings.Split(o.Domains, ",")); err != nil {
			return fmt.Errorf("custom domains: %w", err)
		}
	}

	o.report(len(m.Static), m.HasWorker)
	return nil
}
