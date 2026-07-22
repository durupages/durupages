// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"golang.org/x/term"
)

// secretNameRE is the binding-identifier syntax the admin API enforces for a
// secret name. It is checked client-side too so a typo fails before the value
// ever leaves the machine.
var secretNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// checkSecretName reports whether name is a usable worker binding identifier.
func checkSecretName(name string) error {
	if !secretNameRE.MatchString(name) {
		return fmt.Errorf("invalid secret name %q: names must match %s", name, secretNameRE)
	}
	return nil
}

// utf8BOM is stripped from the head of a secret file: editors on Windows add it
// and it would otherwise become part of the first key.
const utf8BOM = "\xef\xbb\xbf"

// loadSecretFile reads path ("-" means stdin, taken from the given reader) and
// parses it as a secret file. Both `secret bulk --file` and
// `deploy --secrets-file` go through here, so the two accept exactly the same
// documents.
//
// A nil value means the JSON document held an explicit null for that name,
// which `secret bulk` turns into a deletion (wrangler's convention). The
// dotenv format cannot express null. Commands that replace the whole map run
// the result through plainSecrets, which refuses a null.
func loadSecretFile(stdin io.Reader, path string) (map[string]*string, error) {
	var (
		data []byte
		err  error
		what = path
	)
	if path == "-" {
		data, err = io.ReadAll(stdin)
		what = "<stdin>"
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, err
	}
	return parseSecretFile(data, what)
}

// parseSecretFile decodes a secret file in either supported format. The format
// is auto-detected the way wrangler does it: a body whose first non-whitespace
// byte is '{' is JSON, anything else is dotenv.
//
// A completely blank file is rejected rather than read as "no secrets": a
// truncated file must never be mistaken for an instruction. Removing every
// secret is written out as "{}" plus --replace.
func parseSecretFile(data []byte, what string) (map[string]*string, error) {
	body := strings.TrimPrefix(string(data), utf8BOM)
	if strings.TrimSpace(body) == "" {
		return nil, fmt.Errorf("%s: no secrets in this file (to remove every secret write \"{}\" and pass --replace)", what)
	}
	if strings.HasPrefix(strings.TrimSpace(body), "{") {
		return parseSecretJSON(body, what)
	}
	return parseSecretDotenv(body, what)
}

// parseSecretJSON decodes a flat {"NAME":"value"} object. A null value is kept
// as a nil entry: `secret bulk` deletes that secret, mirroring wrangler.
func parseSecretJSON(body, what string) (map[string]*string, error) {
	out := map[string]*string{}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		return nil, fmt.Errorf("%s: %w (expected a flat JSON object of string values)", what, err)
	}
	for name := range out {
		if err := checkSecretName(name); err != nil {
			return nil, fmt.Errorf("%s: %w", what, err)
		}
	}
	return out, nil
}

// parseSecretDotenv decodes the `KEY=value` format. Blank lines and lines whose
// first non-blank character is '#' are ignored; a matching pair of surrounding
// single or double quotes is stripped from the value (which is how a value with
// leading or trailing spaces is written). A line without '=' is an error naming
// its line number, so a stray word cannot be mistaken for a secret.
func parseSecretDotenv(body, what string) (map[string]*string, error) {
	out := map[string]*string{}
	for i, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: %q is not in KEY=value form", what, i+1, line)
		}
		name = strings.TrimSpace(name)
		if err := checkSecretName(name); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", what, i+1, err)
		}
		v := unquoteSecretValue(strings.TrimSpace(value))
		out[name] = &v
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no secrets in this file (to remove every secret write \"{}\" and pass --replace)", what)
	}
	return out, nil
}

// plainSecrets flattens a parsed secret file for the routes that replace the
// whole map, where a null has no meaning: there is nothing to delete when every
// unmentioned secret is dropped anyway. Rather than silently ignoring it, the
// null is reported, since writing one signals an intent (an upsert) that the
// replacing command cannot honour.
func plainSecrets(in map[string]*string, what string) (map[string]string, error) {
	out := make(map[string]string, len(in))
	for name, v := range in {
		if v == nil {
			return nil, fmt.Errorf("%s: secret %q is null, which deletes it; that only makes sense for "+
				"an upsert — drop the key here, or use \"duru secret bulk\" without --replace", what, name)
		}
		out[name] = *v
	}
	return out, nil
}

// unquoteSecretValue strips one matching pair of surrounding quotes.
func unquoteSecretValue(v string) string {
	if len(v) >= 2 {
		q := v[0]
		if (q == '"' || q == '\'') && v[len(v)-1] == q {
			return v[1 : len(v)-1]
		}
	}
	return v
}

// terminalStdin returns c.stdin as an *os.File when it is one and it is a
// terminal. Tests inject a plain reader, which is never a terminal, so they
// always take the piped path.
func (c *cli) terminalStdin() (*os.File, bool) {
	f, ok := c.stdin.(*os.File)
	if !ok {
		return nil, false
	}
	return f, term.IsTerminal(int(f.Fd()))
}

// readSecretValue obtains one secret value, following wrangler: piped or
// redirected stdin is taken verbatim (minus a single trailing newline, so that
// `echo` and a text file both work), and an interactive terminal is prompted on
// stderr and read without echo. The value is never a flag: that would leak it
// into the shell history and into `ps`.
func (c *cli) readSecretValue(name string) (string, error) {
	if f, ok := c.terminalStdin(); ok {
		fmt.Fprintf(c.stderr, "Enter the secret value for %s: ", name)
		raw, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(c.stderr)
		if err != nil {
			return "", fmt.Errorf("read secret value: %w", err)
		}
		return string(raw), nil
	}
	raw, err := io.ReadAll(c.stdin)
	if err != nil {
		return "", fmt.Errorf("read secret value from stdin: %w", err)
	}
	return trimOneNewline(string(raw)), nil
}

// trimOneNewline removes a single trailing line terminator ("\n" or "\r\n"),
// and only one, so a value that deliberately ends in a blank line keeps it.
func trimOneNewline(s string) string {
	if cut, ok := strings.CutSuffix(s, "\n"); ok {
		return strings.TrimSuffix(cut, "\r")
	}
	return s
}
