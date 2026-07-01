// Package ephemeral manages the throwaway environment a backup is restored into.
// It shells out to `docker` so no host Postgres client is required.
package ephemeral

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const (
	pgUser = "postgres"
	pgPass = "salvage"
)

// Postgres is a running throwaway Postgres container.
type Postgres struct {
	ID       string
	Image    string
	Database string
}

// StartPostgres launches a disposable Postgres container and waits for it to
// accept connections. The container is started with --rm so Stop fully removes it.
func StartPostgres(ctx context.Context, image, database string) (*Postgres, error) {
	id, err := run(ctx, "docker", "run", "-d", "--rm",
		"-e", "POSTGRES_USER="+pgUser,
		"-e", "POSTGRES_PASSWORD="+pgPass,
		"-e", "POSTGRES_DB="+database,
		"-P", image,
	)
	if err != nil {
		return nil, fmt.Errorf("start postgres container: %w", err)
	}
	pg := &Postgres{ID: strings.TrimSpace(id), Image: image, Database: database}
	if err := pg.waitReady(ctx); err != nil {
		_ = pg.Stop()
		return nil, err
	}
	return pg, nil
}

func (p *Postgres) waitReady(ctx context.Context) error {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("postgres not ready before timeout: %w", ctx.Err())
		case <-ticker.C:
			// Deliberately NOT pg_isready: the official postgres image runs a
			// temporary socket-only server during initdb that pg_isready reports
			// as ready before the real server (and the target database) exist.
			// That init server has listen_addresses='', so a real query over TCP
			// (127.0.0.1) only succeeds once the actual server is accepting
			// connections — the signal we actually want.
			out, err := p.psql(ctx, "-tAc", "select 1")
			if err == nil && strings.TrimSpace(out) == "1" {
				return nil
			}
		}
	}
}

// Restore loads a backup artifact into the container. kind is "pg_dump"
// (custom/dir/tar via pg_restore) or "sql" (plain SQL via psql). It returns a
// non-empty warning string when the restore completed but pg_restore skipped some
// items — that is reported, not treated as a failure.
func (p *Postgres) Restore(ctx context.Context, kind, path string) (string, error) {
	const dest = "/tmp/salvage-dump"
	if _, err := run(ctx, "docker", "cp", path, p.ID+":"+dest); err != nil {
		return "", fmt.Errorf("copy dump into container: %w", err)
	}
	switch kind {
	case "pg_dump":
		if _, err := p.exec(ctx, "pg_restore", "-h", "127.0.0.1", "-U", pgUser,
			"-d", p.Database, "--no-owner", "--no-privileges", dest); err != nil {
			// pg_restore exits non-zero when it ignores errors but still processes
			// the archive — commonly "schema/object already exists" for extension-
			// managed objects the restore image pre-creates (e.g. PostGIS's tiger /
			// topology schemas). That's a healthy backup with benign noise: surface
			// it as a warning and let the checks judge the data, don't fail outright.
			if strings.Contains(err.Error(), "errors ignored on restore") {
				return restoreWarning(err.Error()), nil
			}
			return "", fmt.Errorf("pg_restore: %w", err)
		}
	case "sql":
		if _, err := p.psql(ctx, "-v", "ON_ERROR_STOP=1", "-f", dest); err != nil {
			return "", fmt.Errorf("psql restore: %w", err)
		}
	default:
		return "", fmt.Errorf("unsupported source kind %q", kind)
	}
	return "", nil
}

// restoreWarning condenses pg_restore's verbose error stream into a short note.
func restoreWarning(stderr string) string {
	n := "some"
	if i := strings.Index(stderr, "errors ignored on restore:"); i >= 0 {
		rest := stderr[i+len("errors ignored on restore:"):]
		n = strings.TrimSpace(strings.SplitN(rest, "\n", 2)[0])
	}
	return "pg_restore ignored " + n + " benign error(s) — e.g. objects already present in the restore image"
}

// Query runs a SQL statement expected to return a single scalar value.
func (p *Postgres) Query(ctx context.Context, sql string) (string, error) {
	return p.psql(ctx, "-tAqc", sql)
}

// psql runs the psql client inside the container against the target database
// over TCP (so it talks to the real server, not the initdb bootstrap server).
func (p *Postgres) psql(ctx context.Context, args ...string) (string, error) {
	base := []string{"psql", "-h", "127.0.0.1", "-U", pgUser, "-d", p.Database}
	return p.exec(ctx, append(base, args...)...)
}

// QueryRows runs a SQL statement and returns its rows, each a slice of column
// strings in SELECT order. Satisfies discover.RowQueryer (used by scaffold).
func (p *Postgres) QueryRows(ctx context.Context, sql string) ([][]string, error) {
	out, err := p.psql(ctx, "-t", "-A", "-F", "\t", "-c", sql)
	if err != nil {
		return nil, err
	}
	return parseRows(out), nil
}

// parseRows splits unaligned, tab-separated psql output into rows of cells.
func parseRows(out string) [][]string {
	out = strings.TrimRight(out, "\n")
	if out == "" {
		return nil
	}
	var rows [][]string
	for _, line := range strings.Split(out, "\n") {
		rows = append(rows, strings.Split(line, "\t"))
	}
	return rows
}

// exec runs a command inside the container with the DB password in the env.
func (p *Postgres) exec(ctx context.Context, args ...string) (string, error) {
	full := append([]string{"exec", "-e", "PGPASSWORD=" + pgPass, p.ID}, args...)
	return run(ctx, "docker", full...)
}

// Stop removes the container. Safe to call more than once.
func (p *Postgres) Stop() error {
	if p == nil || p.ID == "" {
		return nil
	}
	// Use a fresh context so teardown still runs if the parent ctx timed out.
	_, err := run(context.Background(), "docker", "kill", p.ID)
	return err
}

func run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return stdout.String(), fmt.Errorf("%s: %s", name, msg)
	}
	return stdout.String(), nil
}
