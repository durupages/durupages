// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
)

// resource is the kind of object a command works on. Its value is both the name
// of the shared flag that carries the identifier and the noun used in messages.
type resource string

const (
	resNone   resource = ""
	resTenant resource = "tenant"
	resPage   resource = "page"
)

// env names the environment variable that backs the resource's shared flag.
func (r resource) env() string { return "DURUPAGES_" + strings.ToUpper(string(r)) }

// commonFlags are the flags every admin API command accepts: the connection
// flags and the resource identifiers. Identifiers are flags rather than
// positional arguments, so one command line reads the same for every command:
//
//	duru --page blog secret put API_KEY
//
// They may sit before or after the command word (see splitCommand and parse),
// which makes "duru secret put API_KEY --page blog" the same invocation.
type commonFlags struct {
	adminURL string
	token    string
	headers  headerFlag
	timeout  time.Duration

	tenant string
	page   string
}

// register declares the shared flags on fs.
func (g *commonFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&g.tenant, "tenant", envOr("DURUPAGES_TENANT", ""),
		"tenant id the command works on (env DURUPAGES_TENANT)")
	fs.StringVar(&g.page, "page", envOr("DURUPAGES_PAGE", ""),
		"page id the command works on (env DURUPAGES_PAGE)")
	fs.StringVar(&g.adminURL, "admin-url", envOr("DURUPAGES_ADMIN_URL", ""),
		"controller admin API base URL (env DURUPAGES_ADMIN_URL)")
	fs.StringVar(&g.token, "token", envOr("DURUPAGES_ADMIN_TOKEN", ""),
		"bearer token, sent as \"Authorization: Bearer <token>\" (env DURUPAGES_ADMIN_TOKEN)")
	fs.Var(&g.headers, "header",
		"extra request header \"Name: value\"; repeatable, and it overrides --token for the same name")
	fs.DurationVar(&g.timeout, "timeout", defaultTimeout, "HTTP timeout for one request")
}

// sharedValueFlags are the shared flags that take a separate value argument.
// splitCommand skips them to find the command word, which is what lets a shared
// flag precede it.
var sharedValueFlags = map[string]bool{
	"tenant": true, "page": true,
	"admin-url": true, "token": true, "header": true, "timeout": true,
}

// flagName returns the name in a "-name", "--name", "-name=v" or "--name=v"
// argument and whether a value was attached with "=". It returns "" for
// anything that is not a flag, including "-", "--" and a bare positional.
func flagName(arg string) (name string, attached bool) {
	if len(arg) < 2 || arg[0] != '-' {
		return "", false
	}
	s := strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "-")
	if s == "" || strings.HasPrefix(s, "-") {
		return "", false
	}
	if n, _, ok := strings.Cut(s, "="); ok {
		return n, true
	}
	return s, false
}

// splitCommand finds the command word in args and returns it together with the
// remaining arguments, with the shared flags that preceded it moved to the
// front. It is what makes
//
//	duru --page blog secret put API_KEY
//
// dispatch to `secret`, then to `put`, with "--page blog" handed down to the
// leaf command's flag set. An empty name means args held no command word.
func splitCommand(args []string) (string, []string) {
	lead := []string{}
	for i := 0; i < len(args); {
		name, attached := flagName(args[i])
		if name == "" || !sharedValueFlags[name] {
			return args[i], append(lead, args[i+1:]...)
		}
		// "--flag=value" is one argument; "--flag value" is two. A trailing
		// flag with no value is left for the flag package to complain about.
		if attached || i+1 >= len(args) {
			lead = append(lead, args[i])
			i++
			continue
		}
		lead = append(lead, args[i], args[i+1])
		i += 2
	}
	return "", lead
}

// client builds the admin API client, reporting a usage error when no base URL
// was configured.
func (g *commonFlags) client() (*adminClient, error) {
	if strings.TrimSpace(g.adminURL) == "" {
		return nil, usagef("--admin-url is required (or set DURUPAGES_ADMIN_URL)")
	}
	return newAdminClient(g.adminURL, g.token, g.headers.header, g.timeout), nil
}

// id returns the identifier the resource's shared flag holds, empty when
// neither the flag nor its environment variable supplied one.
func (g *commonFlags) id(r resource) string {
	switch r {
	case resTenant:
		return strings.TrimSpace(g.tenant)
	case resPage:
		return strings.TrimSpace(g.page)
	default:
		return ""
	}
}

// require returns the resource's identifier, or a usage error naming both the
// flag and the environment variable that can supply it.
func (g *commonFlags) require(r resource) (string, error) {
	if v := g.id(r); v != "" {
		return v, nil
	}
	return "", usagef("--%s is required (or set %s)", r, r.env())
}

// fileFlags are the --file/-f and --replace flags of the `set` commands.
type fileFlags struct {
	path    string
	replace bool
}

// register declares --file, its -f alias and --replace on fs.
func (f *fileFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.path, "file", "", "read the object from a JSON file in admin API format (\"-\" is stdin)")
	fs.StringVar(&f.path, "f", "", "shorthand for --file")
	fs.BoolVar(&f.replace, "replace", false,
		"do not read the current object first: send exactly file+flags (full replacement)")
}

// given reports whether --file or -f was passed.
func (f *fileFlags) given(set map[string]bool) bool { return set["file"] || set["f"] }

// newFlagSet creates a flag set that never writes to the process streams and
// never exits: parse failures come back as errors so run can map them to an
// exit code.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	return fs
}

// parse parses args and returns the positional arguments. A -h/--help flag
// prints help to stdout and yields errHelp (exit 0); anything else malformed
// yields a usage error (exit 2).
//
// Flags and positional arguments may be interleaved: the flag package stops at
// the first non-flag argument, so each positional is peeled off and parsing
// resumes after it. That is what makes "duru secret put API_KEY --page blog"
// work as well as the documented "duru --page blog secret put API_KEY".
func (c *cli) parse(fs *flag.FlagSet, help string, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				c.printHelp(fs, help)
				return nil, errHelp
			}
			return nil, &usageError{err: err}
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// printHelp writes a command's own usage text and flag list to stdout.
func (c *cli) printHelp(fs *flag.FlagSet, help string) {
	fmt.Fprint(c.stdout, help)
	fmt.Fprintln(c.stdout, "\nflags:")
	fs.SetOutput(c.stdout)
	fs.PrintDefaults()
	fs.SetOutput(io.Discard)
}

// explicitFlags reports which flags were actually present on the command line.
// Precedence is decided with this, never by comparing a flag against its zero
// value, so `--max-concurrency 0` is a real value and not "unset".
func explicitFlags(fs *flag.FlagSet) map[string]bool {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	return set
}

// readSpecFile reads path ("-" means stdin) and decodes the admin API document
// it holds into v.
func (c *cli) readSpecFile(path string, v any) error {
	var (
		data []byte
		err  error
	)
	if path == "-" {
		data, err = io.ReadAll(c.stdin)
		path = "<stdin>"
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return err
	}
	return decodeSpec(data, path, v)
}

// printJSON re-indents an API response with two spaces and writes it to stdout,
// so the output can be piped back in as a --file input.
func (c *cli) printJSON(raw []byte) error {
	var buf bytes.Buffer
	if err := json.Indent(&buf, bytes.TrimSpace(raw), "", "  "); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	buf.WriteByte('\n')
	_, err := c.stdout.Write(buf.Bytes())
	return err
}

// printValue marshals v and writes it to stdout with the same formatting as
// printJSON.
func (c *cli) printValue(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.printJSON(raw)
}

// notef writes one short human-readable line about a mutation to stderr,
// keeping stdout a clean JSON stream.
func (c *cli) notef(format string, args ...any) {
	fmt.Fprintf(c.stderr, format+"\n", args...)
}

// durationString renders d the way the API encodes durations ("30s", "1h30m").
func durationString(d time.Duration) *string {
	s := d.String()
	return &s
}

// writeVerb names what an upsert did, for the one-line stderr note.
func writeVerb(replace, existed bool) string {
	switch {
	case replace:
		return "replaced"
	case existed:
		return "updated"
	default:
		return "created"
	}
}

// stdoutPrint writes s verbatim to stdout.
func (c *cli) stdoutPrint(s string) {
	_, _ = c.stdout.Write([]byte(s))
}

// idRE is what an old-style leading identifier looks like: a tenant or page id,
// never a flag and never a sentence. checkArgs only offers its migration hint
// for an argument shaped like this.
var idRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// checkArgs validates the number of positional arguments a command received.
// n is how many it still takes; use is the one-line usage shown otherwise.
//
// Every resource identifier moved out of the argument list and into the shared
// --tenant/--page flags, so a command that gets exactly one argument too many
// is most likely an old-style invocation. When the extra leading argument looks
// like the identifier that used to sit there, and the flag itself was not
// given, the error spells out the new command line instead of only rejecting
// the count.
func (c *cli) checkArgs(pos []string, n int, use string, g *commonFlags, r resource) error {
	if len(pos) == n {
		return nil
	}
	if r != resNone && len(pos) == n+1 && g.id(r) == "" && idRE.MatchString(pos[0]) {
		rest := strings.TrimSpace(c.cmdName + " " + strings.Join(pos[1:], " "))
		return usagef("unexpected argument %q; the %s is now a flag: duru --%s %s %s",
			pos[n], r, r, pos[0], rest)
	}
	return usagef("usage: %s", use)
}

// healthHelp documents `duru health`.
const healthHelp = `usage: duru health [flags]

GET /healthz on the admin API. Prints {"status":"ok"} when the controller
answers, and fails with exit code 1 otherwise.`

// cmdHealth implements `duru health`.
func (c *cli) cmdHealth(args []string) error {
	fs := newFlagSet("health")
	var g commonFlags
	g.register(fs)
	pos, err := c.parse(fs, healthHelp, args)
	if err != nil {
		return err
	}
	if err := c.checkArgs(pos, 0, "duru health [flags]", &g, resNone); err != nil {
		return err
	}
	client, err := g.client()
	if err != nil {
		return err
	}
	body, err := client.get("/healthz")
	if err != nil {
		return err
	}
	return c.printValue(map[string]string{"status": strings.TrimSpace(string(body))})
}
