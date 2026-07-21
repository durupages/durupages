// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package shim

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// envDuration reads a time.Duration from env, falling back to def.
func envDuration(env string, def time.Duration) time.Duration {
	v := os.Getenv(env)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

// envBytes reads a byte size from env, falling back to def.
func envBytes(env string, def int64) int64 {
	v := os.Getenv(env)
	if v == "" {
		return def
	}
	n, err := parseBytes(v)
	if err != nil {
		return def
	}
	return n
}

// parseBytes parses a byte size with an optional binary (KiB/MiB/GiB/TiB) or
// decimal (KB/MB/GB/TB) suffix; a bare number is bytes.
func parseBytes(s string) (int64, error) {
	s = strings.TrimSpace(s)
	mult := int64(1)
	suffixes := []struct {
		suf  string
		mult int64
	}{
		{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10},
		{"TB", 1e12}, {"GB", 1e9}, {"MB", 1e6}, {"KB", 1e3},
		{"B", 1},
	}
	for _, x := range suffixes {
		if strings.HasSuffix(s, x.suf) {
			mult = x.mult
			s = strings.TrimSpace(strings.TrimSuffix(s, x.suf))
			break
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, err
	}
	return n * mult, nil
}
