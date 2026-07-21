-- Copyright 2026 JC-Lab
-- SPDX-License-Identifier: EPL-2.0

-- Schema for the default PostgreSQL PageProvider. Mirrors ARCHITECTURE 2.4.
-- Uses IF NOT EXISTS throughout so Migrate is idempotent.

CREATE TABLE IF NOT EXISTS tenants (
    id         TEXT PRIMARY KEY,
    config     JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS pages (
    id                   TEXT PRIMARY KEY,
    tenant_id            TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    active_deployment_id TEXT,
    config               JSONB NOT NULL DEFAULT '{}',
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS pages_tenant_idx ON pages(tenant_id);

CREATE TABLE IF NOT EXISTS deployments (
    id         TEXT PRIMARY KEY,
    page_id    TEXT NOT NULL REFERENCES pages(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS custom_domains (
    domain  TEXT PRIMARY KEY,        -- CNAME target domain
    page_id TEXT NOT NULL REFERENCES pages(id) ON DELETE CASCADE
);
