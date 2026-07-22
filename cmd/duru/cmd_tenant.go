// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package main

import "net/url"

// tenantHelp documents the `duru tenant` command group.
const tenantHelp = `usage: duru [--tenant <id>] tenant <subcommand> [flags]

subcommands:
  list                  list every tenant
  get                   show the --tenant tenant
  set                   create or update the --tenant tenant
  delete                delete the tenant, its pages and their deployments
  pages                 list the tenant's pages

Every subcommand but "list" works on the tenant named by the shared --tenant
flag (env DURUPAGES_TENANT):

  duru --tenant acme tenant get

Run "duru tenant <subcommand> -h" for that subcommand's flags.
`

// cmdTenant dispatches the `duru tenant` subcommands.
func (c *cli) cmdTenant(args []string) error {
	sub, rest := splitCommand(args)
	if sub == "" {
		return usagef("missing subcommand\n\n%s", tenantHelp)
	}
	c.cmdName = "tenant " + sub
	switch sub {
	case "-h", "-help", "--help", "help":
		c.stdoutPrint(tenantHelp)
		return errHelp
	case "list":
		return c.tenantList(rest)
	case "get":
		return c.tenantGet(rest)
	case "set":
		return c.tenantSet(rest)
	case "delete":
		return c.tenantDelete(rest)
	case "pages":
		return c.tenantPages(rest)
	default:
		c.cmdName = "tenant"
		return usagef("unknown subcommand %q\n\n%s", sub, tenantHelp)
	}
}

// tenantPath is the API path of one tenant.
func tenantPath(id string) string { return "/v1/tenants/" + url.PathEscape(id) }

const tenantListHelp = `usage: duru tenant list [flags]

GET /v1/tenants. Prints {"tenants":[...]} to stdout. This is the one tenant
subcommand that needs no --tenant.`

// tenantList implements `duru tenant list`.
func (c *cli) tenantList(args []string) error {
	fs := newFlagSet("tenant list")
	var g commonFlags
	g.register(fs)
	pos, err := c.parse(fs, tenantListHelp, args)
	if err != nil {
		return err
	}
	if err := c.checkArgs(pos, 0, "duru tenant list [flags]", &g, resNone); err != nil {
		return err
	}
	client, err := g.client()
	if err != nil {
		return err
	}
	body, err := client.get("/v1/tenants")
	if err != nil {
		return err
	}
	return c.printJSON(body)
}

const tenantGetHelp = `usage: duru --tenant <id> tenant get [flags]

GET /v1/tenants/{id}. The printed document is exactly what "tenant set --file"
accepts, so it round-trips:

  duru --tenant acme tenant get > acme.json
  $EDITOR acme.json
  duru --tenant acme tenant set -f acme.json`

// tenantGet implements `duru tenant get`.
func (c *cli) tenantGet(args []string) error {
	fs := newFlagSet("tenant get")
	var g commonFlags
	g.register(fs)
	pos, err := c.parse(fs, tenantGetHelp, args)
	if err != nil {
		return err
	}
	if err := c.checkArgs(pos, 0, "duru --tenant <id> tenant get [flags]", &g, resTenant); err != nil {
		return err
	}
	id, err := g.require(resTenant)
	if err != nil {
		return err
	}
	client, err := g.client()
	if err != nil {
		return err
	}
	body, err := client.get(tenantPath(id))
	if err != nil {
		return err
	}
	return c.printJSON(body)
}

const tenantSetHelp = `usage: duru --tenant <id> tenant set [flags]

POST /v1/tenants — creates the tenant when it does not exist and updates it
otherwise.

Read-modify-write by default. The API replaces a tenant's whole configuration
on write, so this command first GETs the current tenant (a 404 starts from an
empty one), overlays --file, overlays the flags you actually passed, and posts
the result. --replace skips that read and sends exactly file+flags, which is
how you drop a field back to its default.

Precedence: current server state < --file < explicitly-set flags.

Repeatable k=v flags (--pod-label, --pod-annotation) MERGE into the current map:
"--pod-label a=1" adds or overrides only "a". Combine them with the matching
--clear-* flag to replace a map outright: "--clear-pod-labels --pod-label a=1"
yields exactly {"a":"1"}. A k=v argument without "=" is a usage error.

The file format is the admin API's own JSON, i.e. the output of
"duru --tenant <id> tenant get".`

// tenantSet implements `duru tenant set`.
func (c *cli) tenantSet(args []string) error {
	fs := newFlagSet("tenant set")
	var (
		g      commonFlags
		ff     fileFlags
		labels kvFlag
		annots kvFlag
	)
	g.register(fs)
	ff.register(fs)
	maxConcurrency := fs.Int("max-concurrency", 0, "maximum number of worker pods for this tenant")
	idleTTL := fs.Duration("idle-ttl", 0, "how long an idle worker pod is kept before scale-down")
	cpuLimit := fs.String("cpu-limit", "", "worker pod CPU limit (k8s quantity, e.g. \"1\")")
	memLimit := fs.String("mem-limit", "", "worker pod memory limit (k8s quantity, e.g. \"512Mi\")")
	fs.Var(&labels, "pod-label", "worker pod label \"k=v\"; repeatable, merged into the current labels")
	fs.Var(&annots, "pod-annotation", "worker pod annotation \"k=v\"; repeatable, merged into the current annotations")
	clearLabels := fs.Bool("clear-pod-labels", false, "drop every pod label before applying --pod-label")
	clearAnnots := fs.Bool("clear-pod-annotations", false, "drop every pod annotation before applying --pod-annotation")

	pos, err := c.parse(fs, tenantSetHelp, args)
	if err != nil {
		return err
	}
	if err := c.checkArgs(pos, 0, "duru --tenant <id> tenant set [flags]", &g, resTenant); err != nil {
		return err
	}
	id, err := g.require(resTenant)
	if err != nil {
		return err
	}
	set := explicitFlags(fs)

	client, err := g.client()
	if err != nil {
		return err
	}

	// 1. current server state (unless --replace).
	var spec tenantSpec
	existed := false
	if !ff.replace {
		body, err := client.get(tenantPath(id))
		switch {
		case err == nil:
			if err := decodeSpec(body, "current tenant", &spec); err != nil {
				return err
			}
			existed = true
		case isNotFound(err):
			spec = tenantSpec{}
		default:
			return err
		}
	}

	// 2. --file, for the fields it carries.
	if ff.given(set) {
		var fromFile tenantSpec
		if err := c.readSpecFile(ff.path, &fromFile); err != nil {
			return err
		}
		spec.overlay(fromFile)
	}

	// 3. explicitly-set flags.
	cfg := &spec.Config
	if set["max-concurrency"] {
		cfg.MaxConcurrency = maxConcurrency
	}
	if set["idle-ttl"] {
		cfg.IdleTTL = durationString(*idleTTL)
	}
	if set["cpu-limit"] {
		cfg.WorkerCPULimit = cpuLimit
	}
	if set["mem-limit"] {
		cfg.WorkerMemLimit = memLimit
	}
	cfg.PodLabels = mergeMap(cfg.PodLabels, *clearLabels, labels.pairs)
	cfg.PodAnnotations = mergeMap(cfg.PodAnnotations, *clearAnnots, annots.pairs)
	spec.ID = id

	body, err := client.postSpec("/v1/tenants", spec)
	if err != nil {
		return err
	}
	c.notef("tenant %q %s", id, writeVerb(ff.replace, existed))
	return c.printJSON(body)
}

const tenantDeleteHelp = `usage: duru --tenant <id> tenant delete [flags]

DELETE /v1/tenants/{id}, which also removes the tenant's pages, their
deployments and their custom domains. Deletion is idempotent: removing an
unknown tenant succeeds. Nothing is written to stdout.`

// tenantDelete implements `duru tenant delete`.
func (c *cli) tenantDelete(args []string) error {
	fs := newFlagSet("tenant delete")
	var g commonFlags
	g.register(fs)
	pos, err := c.parse(fs, tenantDeleteHelp, args)
	if err != nil {
		return err
	}
	if err := c.checkArgs(pos, 0, "duru --tenant <id> tenant delete [flags]", &g, resTenant); err != nil {
		return err
	}
	id, err := g.require(resTenant)
	if err != nil {
		return err
	}
	client, err := g.client()
	if err != nil {
		return err
	}
	if err := client.delete(tenantPath(id)); err != nil {
		return err
	}
	c.notef("tenant %q deleted", id)
	return nil
}

const tenantPagesHelp = `usage: duru --tenant <id> tenant pages [flags]

GET /v1/tenants/{id}/pages. Identical to "duru --tenant <id> page list".`

// tenantPages implements `duru tenant pages`.
func (c *cli) tenantPages(args []string) error {
	fs := newFlagSet("tenant pages")
	var g commonFlags
	g.register(fs)
	pos, err := c.parse(fs, tenantPagesHelp, args)
	if err != nil {
		return err
	}
	if err := c.checkArgs(pos, 0, "duru --tenant <id> tenant pages [flags]", &g, resTenant); err != nil {
		return err
	}
	id, err := g.require(resTenant)
	if err != nil {
		return err
	}
	client, err := g.client()
	if err != nil {
		return err
	}
	body, err := client.get(tenantPath(id) + "/pages")
	if err != nil {
		return err
	}
	return c.printJSON(body)
}
