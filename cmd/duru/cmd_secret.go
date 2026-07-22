// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package main

import (
	"net/http"
	"net/url"
)

// secretHelp documents the `duru secret` command group.
const secretHelp = `usage: duru --page <id> secret <subcommand> [flags]

subcommands:
  list                  list the page's secret names (never their values)
  put <NAME>            create or update ONE secret, value read from stdin
  delete <NAME>         remove ONE secret
  bulk --file <path>    replace ALL secrets from a JSON or dotenv file

Every subcommand works on the page named by the shared --page flag (env
DURUPAGES_PAGE):

  duru --page blog secret put API_KEY

"put" and "delete" change a single key: the controller reads the stored map,
applies the change and writes it back, so the other secrets are untouched.
"page set --secret" cannot do that — the API never returns secret values, so a
client has nothing to merge — and replaces the whole map instead.

Secret names must be worker binding identifiers: ^[A-Za-z_][A-Za-z0-9_]*$.

Run "duru secret <subcommand> -h" for that subcommand's flags.
`

// cmdSecret dispatches the `duru secret` subcommands.
func (c *cli) cmdSecret(args []string) error {
	sub, rest := splitCommand(args)
	if sub == "" {
		return usagef("missing subcommand\n\n%s", secretHelp)
	}
	c.cmdName = "secret " + sub
	switch sub {
	case "-h", "-help", "--help", "help":
		c.stdoutPrint(secretHelp)
		return errHelp
	case "list":
		return c.secretList(rest)
	case "put":
		return c.secretPut(rest)
	case "delete":
		return c.secretDelete(rest)
	case "bulk":
		return c.secretBulk(rest)
	default:
		c.cmdName = "secret"
		return usagef("unknown subcommand %q\n\n%s", sub, secretHelp)
	}
}

// secretsPath is the API path of a page's secret collection.
func secretsPath(pageID string) string { return pagePath(pageID) + "/secrets" }

// secretPath is the API path of one named secret.
func secretPath(pageID, name string) string {
	return secretsPath(pageID) + "/" + url.PathEscape(name)
}

const secretListHelp = `usage: duru --page <id> secret list [flags]

GET /v1/pages/{id}/secrets. Prints {"secretKeys":[...]} to stdout — the sorted
names of the page's secrets. Values are write-only in the admin API and are
never returned by any command.`

// secretList implements `duru secret list`.
func (c *cli) secretList(args []string) error {
	fs := newFlagSet("secret list")
	var g commonFlags
	g.register(fs)
	pos, err := c.parse(fs, secretListHelp, args)
	if err != nil {
		return err
	}
	if err := c.checkArgs(pos, 0, "duru --page <id> secret list [flags]", &g, resPage); err != nil {
		return err
	}
	pageID, err := g.require(resPage)
	if err != nil {
		return err
	}
	client, err := g.client()
	if err != nil {
		return err
	}
	body, err := client.get(secretsPath(pageID))
	if err != nil {
		return err
	}
	return c.printJSON(body)
}

const secretPutHelp = `usage: duru --page <id> secret put <NAME> [flags]

PUT /v1/pages/{id}/secrets/{NAME} — creates the secret or overwrites its value,
leaving every other secret of the page alone. The updated page is printed to
stdout; it carries config.secretKeys, never a value.

The value is read from stdin and is never a flag, which would leak it into the
shell history and into "ps":

  printf '%s' "$API_KEY" | duru --page blog secret put API_KEY
  duru --page blog secret put API_KEY < api-key.txt

When stdin is a terminal the command prompts on stderr and reads the value
without echoing it. A single trailing newline is stripped, so "echo v | duru
--page blog secret put ..." stores "v". An empty value is a usage error.`

// secretPut implements `duru secret put`.
func (c *cli) secretPut(args []string) error {
	fs := newFlagSet("secret put")
	var g commonFlags
	g.register(fs)
	pos, err := c.parse(fs, secretPutHelp, args)
	if err != nil {
		return err
	}
	if err := c.checkArgs(pos, 1, "duru --page <id> secret put <NAME> [flags]", &g, resPage); err != nil {
		return err
	}
	pageID, err := g.require(resPage)
	if err != nil {
		return err
	}
	name := pos[0]
	if err := checkSecretName(name); err != nil {
		return &usageError{err: err}
	}
	client, err := g.client()
	if err != nil {
		return err
	}
	value, err := c.readSecretValue(name)
	if err != nil {
		return err
	}
	if value == "" {
		return usagef("the value of secret %q is empty; pipe it in "+
			"(printf '%%s' \"$VALUE\" | duru --page %s secret put %s) or type it at the prompt",
			name, pageID, name)
	}
	body, err := client.putSpec(secretPath(pageID, name), map[string]string{"value": value})
	if err != nil {
		return err
	}
	c.notef("secret %q set on page %q", name, pageID)
	return c.printJSON(body)
}

const secretDeleteHelp = `usage: duru --page <id> secret delete <NAME> [flags]

DELETE /v1/pages/{id}/secrets/{NAME}, which removes that one secret and leaves
the rest of the page's secrets alone. The updated page is printed to stdout.`

// secretDelete implements `duru secret delete`.
func (c *cli) secretDelete(args []string) error {
	fs := newFlagSet("secret delete")
	var g commonFlags
	g.register(fs)
	pos, err := c.parse(fs, secretDeleteHelp, args)
	if err != nil {
		return err
	}
	if err := c.checkArgs(pos, 1, "duru --page <id> secret delete <NAME> [flags]", &g, resPage); err != nil {
		return err
	}
	pageID, err := g.require(resPage)
	if err != nil {
		return err
	}
	name := pos[0]
	if err := checkSecretName(name); err != nil {
		return &usageError{err: err}
	}
	client, err := g.client()
	if err != nil {
		return err
	}
	body, err := client.request(http.MethodDelete, secretPath(pageID, name), nil, "")
	if err != nil {
		return err
	}
	c.notef("secret %q deleted from page %q", name, pageID)
	return c.printJSON(body)
}

const secretBulkHelp = `usage: duru --page <id> secret bulk --file <path> [flags]

Applies a file of secrets to the page and prints the updated page to stdout.

By default this UPSERTS, like "wrangler secret bulk": the secrets in the file are
created or updated and every secret the file does not mention is left alone
(PATCH /v1/pages/{id}/secrets). In a JSON file a null value deletes that one
secret.

With --replace it REPLACES the whole map instead (PUT), so secrets missing from
the file are removed. That is also what "duru deploy --secrets-file" does.

Two file formats are accepted, auto-detected — a body whose first non-blank
character is "{" is JSON, anything else is dotenv:

  {"API_KEY":"s3cr3t","DB_PASSWORD":"hunter2","OLD_KEY":null}

  # dotenv: blank lines and # comments are ignored (it cannot express null)
  API_KEY=s3cr3t
  DB_PASSWORD="hunter2"      # surrounding quotes are stripped

"--file -" reads the file from stdin. A line without "=" is an error naming its
line number, and a blank file is rejected rather than read as "no secrets":
to remove every secret write "{}" and pass --replace. The server caps one bulk
write at 100 entries.`

// secretBulk implements `duru secret bulk`.
func (c *cli) secretBulk(args []string) error {
	fs := newFlagSet("secret bulk")
	var g commonFlags
	g.register(fs)
	var (
		path    string
		replace bool
	)
	fs.StringVar(&path, "file", "", "JSON or dotenv file of secrets to apply (\"-\" is stdin)")
	fs.StringVar(&path, "f", "", "shorthand for --file")
	fs.BoolVar(&replace, "replace", false,
		"replace the whole secret map instead of upserting: secrets missing from the file are removed")
	pos, err := c.parse(fs, secretBulkHelp, args)
	if err != nil {
		return err
	}
	if err := c.checkArgs(pos, 0, "duru --page <id> secret bulk --file <path> [flags]", &g, resPage); err != nil {
		return err
	}
	pageID, err := g.require(resPage)
	if err != nil {
		return err
	}
	if path == "" {
		return usagef("--file <path> is required (\"-\" reads stdin)")
	}
	client, err := g.client()
	if err != nil {
		return err
	}
	secrets, err := loadSecretFile(c.stdin, path)
	if err != nil {
		return err
	}

	if replace {
		// Whole-map write: a null cannot be honoured here, so plainSecrets
		// reports it rather than dropping the key silently.
		plain, err := plainSecrets(secrets, path)
		if err != nil {
			return err
		}
		body, err := client.setSecrets(pageID, plain)
		if err != nil {
			return err
		}
		c.notef("page %q secrets replaced (%d)", pageID, len(plain))
		return c.printJSON(body)
	}

	body, err := client.patchSecrets(pageID, secrets)
	if err != nil {
		return err
	}
	set, removed := 0, 0
	for _, v := range secrets {
		if v == nil {
			removed++
			continue
		}
		set++
	}
	if removed > 0 {
		c.notef("page %q secrets updated (%d set, %d removed)", pageID, set, removed)
	} else {
		c.notef("page %q secrets updated (%d set)", pageID, set)
	}
	return c.printJSON(body)
}
