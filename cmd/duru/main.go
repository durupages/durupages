// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

// Command duru is the DuruPages deploy and administration CLI.
//
// The deploy command has two modes:
//
//	# admin mode — talks to the controller's admin API (no DB/S3 credentials)
//	duru deploy --dir ./build-output --tenant acme --page blog \
//	    --admin-url http://controller:9450
//
//	# direct mode — writes to Storage and the PageProvider itself
//	duru deploy --dir ./build-output --tenant acme --page blog \
//	    --pg-dsn postgres://... --s3-bucket durupages
//
// Both modes scan a wrangler build output directory, upload the deployment
// bundle, register the tenant/page, and atomically switch the page's active
// deployment. Projects using functions/ must be precompiled with
// `wrangler pages functions build` first.
//
// The remaining commands (health, tenant, page, secret, deployment) drive the
// admin API directly, so every operation the controller exposes is scriptable.
// The object a command works on is named by the shared --tenant/--page flags,
// never by a positional argument:
//
//	duru --tenant acme tenant set --max-concurrency 4 --idle-ttl 5m
//	duru --page blog page get > blog.json && duru --page blog page set -f blog.json
//	duru --page blog deployment upload --dir ./build-output --no-activate
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/durupages/durupages/internal/version"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

const usage = `usage: duru deploy [flags]

Admin mode (recommended — no database or object storage credentials needed):
  duru deploy --dir ./build-output --tenant acme --page blog \
      --admin-url http://controller:9450

Direct mode (writes to Storage and PostgreSQL directly):
  duru deploy --dir ./build-output --tenant acme --page blog \
      --pg-dsn postgres://... --s3-bucket durupages

Shipping code and its secrets together (both modes; the file replaces the page's
whole secret set and is applied before the deployment goes live):
  duru deploy --dir ./build-output --tenant acme --page blog \
      --admin-url http://controller:9450 --secrets-file .env.production

Run "duru deploy -h" for the full flag list.
`

// overview lists every command; it is printed for `duru`, `duru -h` and an
// unknown command.
const overview = `duru — the DuruPages deploy and administration CLI

usage: duru [--tenant <id>] [--page <id>] <command> [subcommand] [flags]

The object a command works on is a flag, not an argument:

  duru --page blog secret put API_KEY

deploy:
  deploy --dir <d>              deploy a build output (admin or direct mode)

admin API:
  health                        check the admin API liveness endpoint

  tenant list                   list every tenant
  tenant get                    show the tenant
  tenant set                    create or update the tenant
  tenant delete                 delete the tenant and its pages
  tenant pages                  list the tenant's pages

  page list                     list pages (all tenants, or one with --tenant)
  page get                      show the page
  page set                      create or update the page
  page delete                   delete the page and its deployments
  page domains                  replace the page's custom domain set

  secret list                   list the page's secret names
  secret put <NAME>             set one secret; the value is read from stdin
  secret delete <NAME>          remove one secret
  secret bulk --file <p>        replace every secret from a JSON/dotenv file

  deployment list               list the page's deployments
  deployment upload --dir <d>   upload a build output as a new deployment
  deployment activate <depId>   switch the page's active deployment

Flags shared by every admin command. They may appear before or after the
command, so "duru --page blog secret list" and "duru secret list --page blog"
are the same invocation:
  --tenant ID        tenant id (env DURUPAGES_TENANT)
  --page ID          page id (env DURUPAGES_PAGE)
  --admin-url URL    admin API base URL (env DURUPAGES_ADMIN_URL, required)
  --token TOKEN      sent as "Authorization: Bearer <token>" (env DURUPAGES_ADMIN_TOKEN)
  --header "N: V"    extra request header, repeatable (for non-bearer schemes)
  --timeout D        HTTP timeout (default 10m)

"get", "list" and every mutating command print the API's JSON document to
stdout (2-space indent), so output can be piped straight back in with --file.

Run "duru <command> [subcommand] -h" for that command's own flags.
`

// errHelp is returned by a command whose -h flag was handled; it exits 0.
var errHelp = errors.New("help requested")

// usageError marks a wrong invocation: it exits 2 instead of 1.
type usageError struct{ err error }

func (e *usageError) Error() string { return e.err.Error() }
func (e *usageError) Unwrap() error { return e.err }

// usagef builds a usage error.
func usagef(format string, args ...any) error {
	return &usageError{err: fmt.Errorf(format, args...)}
}

// cli is one CLI invocation. Everything it touches is injectable so the
// commands can be driven from tests.
type cli struct {
	args   []string
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer

	// cmdName is the resolved command path ("tenant set"), used as the error
	// prefix. Dispatch fills it in as it descends.
	cmdName string
}

func main() {
	version.MaybePrint()
	c := &cli{args: os.Args[1:], stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr}
	os.Exit(c.run())
}

// run executes one invocation and returns the process exit code: 0 on success,
// 1 on a runtime or API error, 2 on a usage error.
func (c *cli) run() int {
	if len(c.args) == 0 {
		fmt.Fprint(c.stderr, overview)
		return 2
	}

	// Shared flags may precede the command word, so the command is looked up
	// past them and they are handed down to whatever command was named.
	cmd, args := splitCommand(c.args)
	if cmd == "" {
		fmt.Fprint(c.stderr, "duru: missing command\n\n")
		fmt.Fprint(c.stderr, overview)
		return 2
	}

	switch cmd {
	case "-h", "-help", "--help", "help":
		fmt.Fprint(c.stdout, overview)
		return 0
	case "deploy":
		// Unchanged legacy path: it parses with flag.ExitOnError and reports
		// failures with log.Fatal, so it never returns on error. Its own
		// --tenant/--page flags are the shared ones by name, so both
		// "duru deploy --tenant acme --page blog" and
		// "duru --tenant acme --page blog deploy" reach it unchanged.
		runDeploy(args)
		return 0
	}

	c.cmdName = cmd
	var err error
	switch cmd {
	case "health":
		err = c.cmdHealth(args)
	case "tenant":
		err = c.cmdTenant(args)
	case "page":
		err = c.cmdPage(args)
	case "secret":
		err = c.cmdSecret(args)
	case "deployment":
		err = c.cmdDeployment(args)
	default:
		fmt.Fprintf(c.stderr, "duru: unknown command %q\n\n", cmd)
		fmt.Fprint(c.stderr, overview)
		return 2
	}

	switch {
	case err == nil:
		return 0
	case errors.Is(err, errHelp):
		return 0
	default:
		fmt.Fprintf(c.stderr, "duru %s: %v\n", c.cmdName, err)
		var ue *usageError
		if errors.As(err, &ue) {
			return 2
		}
		return 1
	}
}

// runDeploy is the original `duru deploy` entry point, unchanged.
func runDeploy(args []string) {
	fs := flag.NewFlagSet("deploy", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, usage)
		fmt.Fprintln(os.Stderr, "\nflags:")
		fs.PrintDefaults()
	}
	var (
		dir      = fs.String("dir", ".", "wrangler build output directory")
		tenantID = fs.String("tenant", "", "tenant id (required)")
		pageID   = fs.String("page", "", "page id (required)")
		depID    = fs.String("deployment", "", "deployment id (default: generated)")
		domains  = fs.String("domain", "", "comma-separated custom domains (optional; replaces existing)")
		pagesDom = fs.String("pages-domain", envOr("DURUPAGES_PAGES_DOMAIN", "pages.local"), "pages domain (for the printed URL)")
		secrets  = fs.String("secrets-file", "", "JSON or dotenv file replacing the page's whole secret set, applied before the deployment goes live (\"-\" is stdin)")

		// Admin mode.
		adminURL = fs.String("admin-url", envOr("DURUPAGES_ADMIN_URL", ""), "controller admin API base URL; when set, deploy over HTTP instead of DB/S3")

		// Direct mode.
		pgDSN      = fs.String("pg-dsn", envOr("DURUPAGES_PG_DSN", ""), "PostgreSQL DSN (direct mode)")
		migrate    = fs.Bool("migrate", true, "apply schema migrations before deploying (direct mode)")
		s3Endpoint = fs.String("s3-endpoint", envOr("DURUPAGES_S3_ENDPOINT", ""), "S3 endpoint (direct mode)")
		s3Region   = fs.String("s3-region", envOr("DURUPAGES_S3_REGION", "us-east-1"), "S3 region (direct mode)")
		s3Bucket   = fs.String("s3-bucket", envOr("DURUPAGES_S3_BUCKET", ""), "S3 bucket (direct mode)")
		s3Access   = fs.String("s3-access-key", envOr("DURUPAGES_S3_ACCESS_KEY", ""), "S3 access key (direct mode)")
		s3Secret   = fs.String("s3-secret-key", envOr("DURUPAGES_S3_SECRET_KEY", ""), "S3 secret key (direct mode)")
		s3Path     = fs.Bool("s3-path-style", envOr("DURUPAGES_S3_PATH_STYLE", "true") == "true", "path-style addressing (direct mode)")
	)
	_ = fs.Parse(args)

	if *tenantID == "" || *pageID == "" {
		log.Fatal("duru deploy: --tenant and --page are required")
	}
	if *depID == "" {
		*depID = fmt.Sprintf("dep-%d", time.Now().UnixNano())
	}

	opts := deployOptions{
		Dir:          *dir,
		TenantID:     *tenantID,
		PageID:       *pageID,
		DeploymentID: *depID,
		Domains:      *domains,
		PagesDomain:  *pagesDom,
	}

	// Parse the secret file up front: a typo in it must fail before anything
	// has been uploaded or created.
	if *secrets != "" {
		parsed, err := loadSecretFile(os.Stdin, *secrets)
		if err != nil {
			log.Fatalf("duru deploy: --secrets-file: %v", err)
		}
		// --secrets-file replaces the whole map, so a null (which means
		// "delete this one") cannot be honoured; plainSecrets rejects it.
		set, err := plainSecrets(parsed, *secrets)
		if err != nil {
			log.Fatalf("duru deploy: --secrets-file: %v", err)
		}
		opts.Secrets = &set
	}

	var err error
	switch {
	case *adminURL != "":
		err = deployViaAdmin(opts, *adminURL)
	case *pgDSN != "" && *s3Bucket != "":
		err = deployDirect(opts, directOptions{
			PGDSN:      *pgDSN,
			Migrate:    *migrate,
			S3Endpoint: *s3Endpoint, S3Region: *s3Region, S3Bucket: *s3Bucket,
			S3AccessKey: *s3Access, S3SecretKey: *s3Secret, S3PathStyle: *s3Path,
		})
	default:
		log.Fatal("duru deploy: set --admin-url (admin mode), or --pg-dsn and --s3-bucket (direct mode)")
	}
	if err != nil {
		log.Fatalf("duru deploy: %v", err)
	}
}

// deployOptions are the mode-independent inputs of a deploy.
//
// Secrets is nil unless --secrets-file was given; when it is set it is the
// page's complete new secret map, applied before the deployment goes live.
type deployOptions struct {
	Dir          string
	TenantID     string
	PageID       string
	DeploymentID string
	Domains      string
	PagesDomain  string
	Secrets      *map[string]string
}

// report prints the result of a successful deploy.
func (o deployOptions) report(staticCount int, hasWorker bool) {
	fmt.Printf("deployed %s/%s deployment=%s static=%d worker=%v\n",
		o.TenantID, o.PageID, o.DeploymentID, staticCount, hasWorker)
	fmt.Printf("url: https://%s.%s/\n", o.PageID, o.PagesDomain)
}

// reportSecrets notes a --secrets-file replacement on stderr, keeping the
// deploy summary on stdout unchanged. Only the count is printed: no command
// ever writes a secret value anywhere.
func (o deployOptions) reportSecrets(n int) {
	fmt.Fprintf(os.Stderr, "secrets replaced (%d) on page %q\n", n, o.PageID)
}
