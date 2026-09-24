// Package version shortens raw server version strings for UI display.
// S1-UI-07: tidb_version() returns a 15+ line build blob and PostgreSQL's
// SELECT version() a long single line with build details — both were shown
// verbatim in test-connection toasts. These helpers reduce them to a compact
// "<product> <version>" with a first-line fallback for unexpected input.
package version

import (
	"regexp"
	"strings"
)

var pgVersionRe = regexp.MustCompile(`PostgreSQL \d+(?:\.\d+)*`)

var tidbVersionRe = regexp.MustCompile(`Release Version: (v\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?)`)

func firstLine(s string) string {
	s = strings.TrimSpace(s) // drop leading blank lines before splitting
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// ShortPostgreSQL trims build suffixes: "PostgreSQL 16.15 (Debian …)" →
// "PostgreSQL 16.15". Unparseable input falls back to its first line.
func ShortPostgreSQL(v string) string {
	if m := pgVersionRe.FindString(v); m != "" {
		return m
	}
	return firstLine(v)
}

// ShortTiDB extracts the release version from the tidb_version() blob →
// "TiDB v7.1.9". Unparseable input falls back to the blob's first line.
func ShortTiDB(blob string) string {
	if m := tidbVersionRe.FindStringSubmatch(blob); len(m) > 1 {
		return "TiDB " + string(m[1])
	}
	return firstLine(blob)
}
