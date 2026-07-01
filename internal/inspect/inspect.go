// Package inspect performs offline pre-flight inspection of a restored
// PostgreSQL data directory (PGDATA) without starting a server.
//
// It reads the plain-text files that a stopped cluster leaves on disk — the
// PG_VERSION marker, the configuration files, and the per-database
// subdirectories under base/ — so an operator can learn what it takes to bring
// a backup back before committing to an (expensive) cluster start. This
// implements spec 0001 requirements R1 (offline pre-flight) and R6
// (salvage inspect). It depends on the standard library only.
package inspect

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Result is the outcome of inspecting a PGDATA directory offline.
type Result struct {
	// PGVersion is the PostgreSQL major version from PG_VERSION (e.g. "17").
	PGVersion string `json:"pg_version"`
	// RequiredPreloadExtensions is the effective shared_preload_libraries set —
	// the extensions that must be present for the cluster to start. Empty when
	// no preload libraries are configured.
	RequiredPreloadExtensions []string `json:"required_preload_extensions"`
	// DatabaseCount is the number of database subdirectories under base/.
	DatabaseCount int `json:"database_count"`
}

// Inspect reads a restored PGDATA directory and reports the environment it
// needs to start. It does not start Postgres; everything is parsed from files
// on disk. A missing or unreadable PG_VERSION is an error (the path is almost
// certainly not a PGDATA directory); missing config files and a missing base/
// directory are tolerated and reported as empty/zero.
func Inspect(pgdata string) (*Result, error) {
	version, err := readVersion(filepath.Join(pgdata, "PG_VERSION"))
	if err != nil {
		return nil, err
	}

	preload, err := readPreloadLibraries(pgdata)
	if err != nil {
		return nil, err
	}

	count, err := countDatabases(filepath.Join(pgdata, "base"))
	if err != nil {
		return nil, err
	}

	return &Result{
		PGVersion:                 version,
		RequiredPreloadExtensions: preload,
		DatabaseCount:             count,
	}, nil
}

// readVersion reads the PostgreSQL major version from the PG_VERSION marker.
func readVersion(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read PG_VERSION: %w", err)
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return "", fmt.Errorf("PG_VERSION at %s is empty", path)
	}
	return v, nil
}

// readPreloadLibraries computes the effective shared_preload_libraries by
// reading postgresql.conf first and then letting postgresql.auto.conf override
// it (auto.conf is rewritten by ALTER SYSTEM and wins). Within a single file
// the last matching assignment wins. A missing file is treated as "no setting".
func readPreloadLibraries(pgdata string) ([]string, error) {
	value := ""
	found := false

	for _, name := range []string{"postgresql.conf", "postgresql.auto.conf"} {
		v, ok, err := scanPreloadSetting(filepath.Join(pgdata, name))
		if err != nil {
			return nil, err
		}
		if ok {
			value = v
			found = true
		}
	}

	if !found {
		return nil, nil
	}
	return splitLibraries(value), nil
}

// scanPreloadSetting returns the last shared_preload_libraries value in a conf
// file. ok is false when the file is absent or the setting never appears. A
// file that exists but cannot be read is an error.
func scanPreloadSetting(path string) (value string, ok bool, err error) {
	f, ferr := os.Open(path)
	if ferr != nil {
		if os.IsNotExist(ferr) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("open %s: %w", filepath.Base(path), ferr)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := stripComment(sc.Text())
		key, val, isSetting := parseSetting(line)
		if !isSetting || key != "shared_preload_libraries" {
			continue
		}
		value, ok = val, true
	}
	if serr := sc.Err(); serr != nil {
		return "", false, fmt.Errorf("read %s: %w", filepath.Base(path), serr)
	}
	return value, ok, nil
}

// parseSetting splits a "key = value" configuration line. It reports isSetting
// false for blank lines or lines without an '=' separator.
func parseSetting(line string) (key, value string, isSetting bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return "", "", false
	}
	eq := strings.IndexByte(line, '=')
	if eq < 0 {
		return "", "", false
	}
	key = strings.ToLower(strings.TrimSpace(line[:eq]))
	value = unquote(strings.TrimSpace(line[eq+1:]))
	return key, value, true
}

// stripComment removes an unquoted '#' comment from a configuration line.
func stripComment(line string) string {
	inSingle := false
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '\'':
			inSingle = !inSingle
		case '#':
			if !inSingle {
				return line[:i]
			}
		}
	}
	return line
}

// unquote removes a single layer of surrounding single quotes from a value.
func unquote(s string) string {
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		return s[1 : len(s)-1]
	}
	return s
}

// splitLibraries turns a shared_preload_libraries value into a cleaned list,
// splitting on commas and dropping surrounding whitespace and empty entries.
func splitLibraries(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		p = strings.Trim(p, "'\"")
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// countDatabases counts the per-database subdirectories under base/. Each
// database in a cluster is a directory named after its OID. A missing base/
// directory yields zero (e.g. when inspecting a partial materialization).
func countDatabases(base string) (int, error) {
	entries, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read base/: %w", err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// Database directories are named by numeric OID; skip anything else
		// (e.g. pgsql_tmp) so the count reflects real databases.
		if _, perr := strconv.Atoi(e.Name()); perr != nil {
			continue
		}
		n++
	}
	return n, nil
}
