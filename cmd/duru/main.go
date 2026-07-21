// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

// Command duru is the DuruPages deploy CLI.
//
//	duru deploy --dir ./build-output --tenant acme --page blog \
//	    --pg-dsn postgres://... --s3-bucket durupages [--domain www.example.com]
//
// It scans a wrangler build output directory, uploads the deployment bundle
// to Storage, registers the tenant/page in the PageProvider, and atomically
// switches the page's active deployment. Projects using functions/ must be
// precompiled with `wrangler pages functions build` first.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/durupages/durupages/internal/version"
	"github.com/durupages/durupages/pkg/bundle"
	"github.com/durupages/durupages/pkg/provider"
	"github.com/durupages/durupages/pkg/provider/postgres"
	"github.com/durupages/durupages/pkg/storage/s3"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	version.MaybePrint()
	if len(os.Args) < 2 || os.Args[1] != "deploy" {
		fmt.Fprintln(os.Stderr, "usage: duru deploy [flags]")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("deploy", flag.ExitOnError)
	var (
		dir        = fs.String("dir", ".", "wrangler build output directory")
		tenantID   = fs.String("tenant", "", "tenant id (required)")
		pageID     = fs.String("page", "", "page id (required)")
		depID      = fs.String("deployment", "", "deployment id (default: generated)")
		domains    = fs.String("domain", "", "comma-separated custom domains (optional; replaces existing)")
		pagesDom   = fs.String("pages-domain", envOr("DURUPAGES_PAGES_DOMAIN", "pages.local"), "pages domain")
		pgDSN      = fs.String("pg-dsn", envOr("DURUPAGES_PG_DSN", ""), "PostgreSQL DSN (required)")
		migrate    = fs.Bool("migrate", true, "apply schema migrations before deploying")
		s3Endpoint = fs.String("s3-endpoint", envOr("DURUPAGES_S3_ENDPOINT", ""), "S3 endpoint")
		s3Region   = fs.String("s3-region", envOr("DURUPAGES_S3_REGION", "us-east-1"), "S3 region")
		s3Bucket   = fs.String("s3-bucket", envOr("DURUPAGES_S3_BUCKET", ""), "S3 bucket (required)")
		s3Access   = fs.String("s3-access-key", envOr("DURUPAGES_S3_ACCESS_KEY", ""), "S3 access key")
		s3Secret   = fs.String("s3-secret-key", envOr("DURUPAGES_S3_SECRET_KEY", ""), "S3 secret key")
		s3Path     = fs.Bool("s3-path-style", envOr("DURUPAGES_S3_PATH_STYLE", "true") == "true", "path-style addressing")
	)
	_ = fs.Parse(os.Args[2:])
	if *tenantID == "" || *pageID == "" || *pgDSN == "" || *s3Bucket == "" {
		log.Fatal("duru deploy: --tenant, --page, --pg-dsn and --s3-bucket are required")
	}
	if *depID == "" {
		*depID = fmt.Sprintf("dep-%d", time.Now().UnixNano())
	}

	ctx := context.Background()
	store, err := s3.New(ctx, s3.Options{Endpoint: *s3Endpoint, Region: *s3Region, Bucket: *s3Bucket,
		AccessKey: *s3Access, SecretKey: *s3Secret, UsePathStyle: *s3Path})
	if err != nil {
		log.Fatalf("duru deploy: s3: %v", err)
	}
	prov, err := postgres.New(ctx, postgres.Options{DSN: *pgDSN, PagesDomain: *pagesDom})
	if err != nil {
		log.Fatalf("duru deploy: postgres: %v", err)
	}
	defer prov.Close()
	if *migrate {
		if err := prov.Migrate(ctx); err != nil {
			log.Fatalf("duru deploy: migrate: %v", err)
		}
	}

	m, err := bundle.Deploy(ctx, store, *dir, bundle.ScanOptions{
		TenantID: *tenantID, PageID: *pageID, DeploymentID: *depID,
	})
	if err != nil {
		log.Fatalf("duru deploy: %v", err)
	}

	// Create tenant/page only when missing so redeploys keep existing config.
	if _, err := prov.GetTenant(ctx, *tenantID); errors.Is(err, provider.ErrNotFound) {
		if err := prov.UpsertTenant(ctx, provider.Tenant{ID: *tenantID}); err != nil {
			log.Fatalf("duru deploy: upsert tenant: %v", err)
		}
	} else if err != nil {
		log.Fatalf("duru deploy: get tenant: %v", err)
	}
	if _, err := prov.GetPage(ctx, *pageID); errors.Is(err, provider.ErrNotFound) {
		if err := prov.UpsertPage(ctx, provider.Page{ID: *pageID, TenantID: *tenantID}); err != nil {
			log.Fatalf("duru deploy: upsert page: %v", err)
		}
	} else if err != nil {
		log.Fatalf("duru deploy: get page: %v", err)
	}
	if err := prov.CreateDeployment(ctx, provider.Deployment{ID: *depID, PageID: *pageID, CreatedAt: time.Now()}); err != nil {
		log.Fatalf("duru deploy: create deployment: %v", err)
	}
	if err := prov.SetActiveDeployment(ctx, *pageID, *depID); err != nil {
		log.Fatalf("duru deploy: activate: %v", err)
	}
	if *domains != "" {
		list := strings.Split(*domains, ",")
		if err := prov.SetCustomDomains(ctx, *pageID, list); err != nil {
			log.Fatalf("duru deploy: custom domains: %v", err)
		}
	}

	fmt.Printf("deployed %s/%s deployment=%s static=%d worker=%v\n",
		*tenantID, *pageID, *depID, len(m.Static), m.HasWorker)
	fmt.Printf("url: https://%s.%s/\n", *pageID, *pagesDom)
}
