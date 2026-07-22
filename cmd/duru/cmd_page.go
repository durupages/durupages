// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package main

import "net/url"

// pageHelp documents the `duru page` command group.
const pageHelp = `usage: duru [--page <id>] page <subcommand> [flags]

subcommands:
  list                  list every page, or only one tenant's with --tenant
  get                   show the --page page
  set                   create or update the --page page (--tenant to create)
  delete                delete the page and its deployments
  domains               replace the page's custom domain set

Every subcommand but "list" works on the page named by the shared --page flag
(env DURUPAGES_PAGE):

  duru --page blog page get

Run "duru page <subcommand> -h" for that subcommand's flags.
`

// cmdPage dispatches the `duru page` subcommands.
func (c *cli) cmdPage(args []string) error {
	sub, rest := splitCommand(args)
	if sub == "" {
		return usagef("missing subcommand\n\n%s", pageHelp)
	}
	c.cmdName = "page " + sub
	switch sub {
	case "-h", "-help", "--help", "help":
		c.stdoutPrint(pageHelp)
		return errHelp
	case "list":
		return c.pageList(rest)
	case "get":
		return c.pageGet(rest)
	case "set":
		return c.pageSet(rest)
	case "delete":
		return c.pageDelete(rest)
	case "domains":
		return c.pageDomains(rest)
	default:
		c.cmdName = "page"
		return usagef("unknown subcommand %q\n\n%s", sub, pageHelp)
	}
}

// pagePath is the API path of one page.
func pagePath(id string) string { return "/v1/pages/" + url.PathEscape(id) }

const pageListHelp = `usage: duru [--tenant <id>] page list [flags]

GET /v1/pages, or GET /v1/tenants/{id}/pages when --tenant (env
DURUPAGES_TENANT) names one. Prints {"pages":[...]} to stdout.`

// pageList implements `duru page list`.
func (c *cli) pageList(args []string) error {
	fs := newFlagSet("page list")
	var g commonFlags
	g.register(fs)
	pos, err := c.parse(fs, pageListHelp, args)
	if err != nil {
		return err
	}
	if err := c.checkArgs(pos, 0, "duru [--tenant <id>] page list [flags]", &g, resNone); err != nil {
		return err
	}
	client, err := g.client()
	if err != nil {
		return err
	}
	// --tenant is a filter here, not a requirement: without it every page is
	// listed.
	path := "/v1/pages"
	if tenant := g.id(resTenant); tenant != "" {
		path = tenantPath(tenant) + "/pages"
	}
	body, err := client.get(path)
	if err != nil {
		return err
	}
	return c.printJSON(body)
}

const pageGetHelp = `usage: duru --page <id> page get [flags]

GET /v1/pages/{id}. Secret values are write-only in the admin API and are never
returned; the response instead lists their names in config.secretKeys.

The printed document is exactly what "page set --file" accepts:

  duru --page blog page get > blog.json
  $EDITOR blog.json
  duru --page blog page set -f blog.json`

// pageGet implements `duru page get`.
func (c *cli) pageGet(args []string) error {
	fs := newFlagSet("page get")
	var g commonFlags
	g.register(fs)
	pos, err := c.parse(fs, pageGetHelp, args)
	if err != nil {
		return err
	}
	if err := c.checkArgs(pos, 0, "duru --page <id> page get [flags]", &g, resPage); err != nil {
		return err
	}
	id, err := g.require(resPage)
	if err != nil {
		return err
	}
	client, err := g.client()
	if err != nil {
		return err
	}
	body, err := client.get(pagePath(id))
	if err != nil {
		return err
	}
	return c.printJSON(body)
}

const pageSetHelp = `usage: duru --page <id> [--tenant <id>] page set [flags]

POST /v1/pages — creates the page when it does not exist and updates it
otherwise. --tenant (env DURUPAGES_TENANT) is required when creating; an
existing page keeps its tenant when the flag is left out.

Read-modify-write by default. The API replaces config.env, config.queueTimeout,
config.requestTimeout and config.logsEnabled on write, so a naive partial POST
would silently wipe them. This command therefore first GETs the current page (a
404 starts from an empty one), overlays --file, overlays the flags you actually
passed, and posts the result. --replace skips that read and sends exactly
file+flags, which is how you drop a field back to its default.

Precedence: current server state < --file < explicitly-set flags.

Repeatable k=v flags (--env, --secret) MERGE into the current map: "--env A=1"
adds or overrides only A. Combine them with --clear-env / --clear-secrets to
replace a map outright: "--clear-env --env A=1" yields exactly {"A":"1"}.
Repeatable list flags (--domain) REPLACE the whole list. A k=v argument without
"=" is a usage error.

Secrets are write-only: the API never returns their values, so the CLI cannot
merge them with what the server already stores. Passing --secret therefore
sends the set you give (plus any secrets in --file) and REPLACES the stored
secrets; leaving --secret out keeps them untouched.

--logs-enabled is tri-state and always takes an argument: leaving it out keeps
the stored value (null = follow the global setting), "--logs-enabled false"
explicitly disables log ingest.

The file format is the admin API's own JSON, i.e. the output of
"duru --page <id> page get"; a config.secretKeys field in it is accepted and
ignored.`

// pageSet implements `duru page set`.
func (c *cli) pageSet(args []string) error {
	fs := newFlagSet("page set")
	var (
		g       commonFlags
		ff      fileFlags
		env     kvFlag
		secret  kvFlag
		domains listFlag
		logs    boolFlag
	)
	g.register(fs)
	ff.register(fs)
	queueTimeout := fs.Duration("queue-timeout", 0, "how long a request may wait for a worker")
	requestTimeout := fs.Duration("request-timeout", 0, "how long a worker may take to answer")
	fs.Var(&env, "env", "environment binding \"k=v\"; repeatable, merged into the current env")
	fs.Var(&secret, "secret", "secret binding \"k=v\"; repeatable, write-only (see above)")
	fs.Var(&logs, "logs-enabled", "log ingest override, \"true\" or \"false\" (unset keeps the stored value)")
	fs.Var(&domains, "domain", "custom domain; repeatable, and the given list replaces the stored one")
	clearEnv := fs.Bool("clear-env", false, "drop every env binding before applying --env")
	clearSecrets := fs.Bool("clear-secrets", false, "drop every secret before applying --secret")
	clearDomains := fs.Bool("clear-domains", false, "remove every custom domain")

	pos, err := c.parse(fs, pageSetHelp, args)
	if err != nil {
		return err
	}
	if err := c.checkArgs(pos, 0, "duru --page <id> [--tenant <id>] page set [flags]", &g, resPage); err != nil {
		return err
	}
	id, err := g.require(resPage)
	if err != nil {
		return err
	}
	set := explicitFlags(fs)

	client, err := g.client()
	if err != nil {
		return err
	}

	// 1. current server state (unless --replace).
	var spec pageSpec
	existed := false
	if !ff.replace {
		body, err := client.get(pagePath(id))
		switch {
		case err == nil:
			if err := decodeSpec(body, "current page", &spec); err != nil {
				return err
			}
			existed = true
		case isNotFound(err):
			spec = pageSpec{}
		default:
			return err
		}
	}

	// 2. --file, for the fields it carries.
	if ff.given(set) {
		var fromFile pageSpec
		if err := c.readSpecFile(ff.path, &fromFile); err != nil {
			return err
		}
		spec.overlay(fromFile)
	}

	// 3. explicitly-set flags. --tenant is the shared flag, so an owning tenant
	// may equally come from DURUPAGES_TENANT.
	if tenant := g.id(resTenant); tenant != "" {
		spec.TenantID = tenant
	}
	cfg := &spec.Config
	if set["queue-timeout"] {
		cfg.QueueTimeout = durationString(*queueTimeout)
	}
	if set["request-timeout"] {
		cfg.RequestTimeout = durationString(*requestTimeout)
	}
	if logs.value != nil {
		cfg.LogsEnabled = logs.value
	}
	cfg.Env = mergeMap(cfg.Env, *clearEnv, env.pairs)
	cfg.Secret = mergeMap(cfg.Secret, *clearSecrets, secret.pairs)
	switch {
	case len(domains.values) > 0:
		list := append([]string(nil), domains.values...)
		spec.CustomDomains = &list
	case *clearDomains:
		spec.CustomDomains = &[]string{}
	}
	spec.ID = id

	if spec.TenantID == "" {
		return usagef("page %q does not exist yet: --tenant <id> is required to "+
			"create it (or set DURUPAGES_TENANT)", id)
	}

	body, err := client.postSpec("/v1/pages", spec.forRequest())
	if err != nil {
		return err
	}
	c.notef("page %q %s", id, writeVerb(ff.replace, existed))
	return c.printJSON(body)
}

const pageDeleteHelp = `usage: duru --page <id> page delete [flags]

DELETE /v1/pages/{id}, which also removes the page's deployments and custom
domains. Deletion is idempotent: removing an unknown page succeeds. Nothing is
written to stdout.`

// pageDelete implements `duru page delete`.
func (c *cli) pageDelete(args []string) error {
	fs := newFlagSet("page delete")
	var g commonFlags
	g.register(fs)
	pos, err := c.parse(fs, pageDeleteHelp, args)
	if err != nil {
		return err
	}
	if err := c.checkArgs(pos, 0, "duru --page <id> page delete [flags]", &g, resPage); err != nil {
		return err
	}
	id, err := g.require(resPage)
	if err != nil {
		return err
	}
	client, err := g.client()
	if err != nil {
		return err
	}
	if err := client.delete(pagePath(id)); err != nil {
		return err
	}
	c.notef("page %q deleted", id)
	return nil
}

const pageDomainsHelp = `usage: duru --page <id> page domains --domain d [--domain d ...]
       duru --page <id> page domains --clear

PUT /v1/pages/{id}/custom-domains, which replaces the page's whole domain set:
the domains you pass are the domains the page will have. Give --clear to remove
them all. One of --domain or --clear is required, so the set can never be wiped
by an empty invocation. The updated page is printed to stdout.`

// pageDomains implements `duru page domains`.
func (c *cli) pageDomains(args []string) error {
	fs := newFlagSet("page domains")
	var (
		g       commonFlags
		domains listFlag
	)
	g.register(fs)
	fs.Var(&domains, "domain", "custom domain; repeatable, and the given list replaces the stored one")
	clear := fs.Bool("clear", false, "remove every custom domain")
	pos, err := c.parse(fs, pageDomainsHelp, args)
	if err != nil {
		return err
	}
	if err := c.checkArgs(pos, 0, "duru --page <id> page domains [--domain d ...] [--clear]", &g, resPage); err != nil {
		return err
	}
	id, err := g.require(resPage)
	if err != nil {
		return err
	}
	if len(domains.values) == 0 && !*clear {
		return usagef("give --domain <d> (repeatable) or --clear; " +
			"this command replaces the whole domain set")
	}
	client, err := g.client()
	if err != nil {
		return err
	}
	// --clear on its own sends the empty set; --domain wins when both appear.
	list := append([]string{}, domains.values...)
	body, err := client.setCustomDomains(id, list)
	if err != nil {
		return err
	}
	c.notef("page %q custom domains updated (%d)", id, len(list))
	return c.printJSON(body)
}
