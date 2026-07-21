// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package shim

import (
	"encoding/base64"
	"net/url"
	"testing"

	"github.com/durupages/durupages/pkg/usage"
)

func TestRedactStringTable(t *testing.T) {
	secret := "supersecretvalue"
	r := newRedactor(map[string]string{
		"TOKEN": secret,
		"SHORT": "abc", // below minSecretLen, ignored
	})

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"raw", "prefix " + secret + " suffix", "prefix [REDACTED:TOKEN] suffix"},
		{"url-encoded", "x=" + url.QueryEscape(secret), "x=[REDACTED:TOKEN]"},
		{"base64-std", base64.StdEncoding.EncodeToString([]byte(secret)), "[REDACTED:TOKEN]"},
		{"base64-raw", base64.RawStdEncoding.EncodeToString([]byte(secret)), "[REDACTED:TOKEN]"},
		{"no-match", "nothing here", "nothing here"},
		{"short-secret-ignored", "value abc here", "value abc here"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := r.redactString(tc.in); got != tc.want {
				t.Errorf("redactString(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRedactHeadersTable(t *testing.T) {
	secret := "supersecretvalue"
	r := newRedactor(map[string]string{"TOKEN": secret})

	headers := map[string]string{
		"Authorization": "Bearer xyz",
		"Cookie":        "session=1",
		"Set-Cookie":    "a=b",
		"X-Api-Key":     "k",
		"User-Agent":    "curl/8 " + secret,
		"Content-Type":  "application/json",
	}
	r.redactHeaders(headers)

	wantRedacted := []string{"Authorization", "Cookie", "Set-Cookie", "X-Api-Key"}
	for _, name := range wantRedacted {
		if headers[name] != "[REDACTED]" {
			t.Errorf("%s = %q, want [REDACTED]", name, headers[name])
		}
	}
	if headers["User-Agent"] != "curl/8 [REDACTED:TOKEN]" {
		t.Errorf("User-Agent = %q, secret not redacted from value", headers["User-Agent"])
	}
	if headers["Content-Type"] != "application/json" {
		t.Errorf("Content-Type unexpectedly modified: %q", headers["Content-Type"])
	}
}

func TestRedactApply(t *testing.T) {
	secret := "supersecretvalue"
	r := newRedactor(map[string]string{"TOKEN": secret})
	u := usage.RequestUsage{
		Event: usage.Event{
			Request: usage.RequestInfo{Headers: map[string]string{"Authorization": "Bearer t"}},
		},
		Logs:       []usage.LogEntry{{Message: "leak " + secret}},
		Exceptions: []usage.Exception{{Message: "boom " + secret, Stack: "at " + secret}},
	}
	r.apply(&u)
	if u.Logs[0].Message != "leak [REDACTED:TOKEN]" {
		t.Errorf("log = %q", u.Logs[0].Message)
	}
	if u.Exceptions[0].Message != "boom [REDACTED:TOKEN]" || u.Exceptions[0].Stack != "at [REDACTED:TOKEN]" {
		t.Errorf("exception = %+v", u.Exceptions[0])
	}
	if u.Event.Request.Headers["Authorization"] != "[REDACTED]" {
		t.Errorf("header not redacted: %q", u.Event.Request.Headers["Authorization"])
	}
}
