// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

// Package version exposes build-time version information. The values are
// injected at build time with -ldflags:
//
//	go build -ldflags "\
//	  -X github.com/durupages/durupages/internal/version.Version=v0.1.2 \
//	  -X github.com/durupages/durupages/internal/version.Commit=$(git rev-parse HEAD) \
//	  -X github.com/durupages/durupages/internal/version.Date=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
//
// When not injected (e.g. `go run`), it falls back to the module's VCS build
// info so a locally built binary still reports something useful.
package version

import (
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
)

// Injected via -ldflags at release build time. Defaults apply otherwise.
var (
	Version = "dev"
	Commit  = ""
	Date    = ""
)

// info lazily fills Commit/Date from VCS build info when they were not injected.
func resolve() (v, commit, date string) {
	v, commit, date = Version, Commit, Date
	if commit != "" && date != "" {
		return v, commit, date
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return v, commit, date
	}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			if commit == "" {
				commit = s.Value
			}
		case "vcs.time":
			if date == "" {
				date = s.Value
			}
		}
	}
	return v, commit, date
}

// String returns a one-line human-readable version string.
func String() string {
	v, commit, date := resolve()
	s := v
	if commit != "" {
		short := commit
		if len(short) > 12 {
			short = short[:12]
		}
		s += " (" + short + ")"
	}
	if date != "" {
		s += " built " + date
	}
	s += " " + runtime.GOOS + "/" + runtime.GOARCH
	return s
}

// MaybePrint prints the version and exits(0) when the process was invoked with
// a version request (`--version`, `-version`, or a leading `version` argument).
// Call it as the first line of main so it works uniformly for binaries that use
// the flag package and for those that parse os.Args directly.
func MaybePrint() {
	for i, a := range os.Args[1:] {
		switch a {
		case "--version", "-version":
			fmt.Println(String())
			os.Exit(0)
		case "version":
			// Only treat a *leading* `version` token as the subcommand, so it
			// does not swallow a value that legitimately equals "version".
			if i == 0 {
				fmt.Println(String())
				os.Exit(0)
			}
		}
	}
}
