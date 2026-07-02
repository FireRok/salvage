package ephemeral

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const (
	mysqlUser = "root"
	mysqlPass = "salvage"
)

// MySQL is a running throwaway MySQL container. Unlike Postgres there is no Go
// driver dependency here either: every interaction shells to the `mysql` CLI
// inside the container via `docker exec`, mirroring how internal/ephemeral/
// postgres.go drives psql.
type MySQL struct {
	ID       string
	Image    string
	Database string
}

// StartMySQL launches a disposable MySQL container and waits for it to accept
// connections. The container is started with --rm so Stop fully removes it.
func StartMySQL(ctx context.Context, image, database string) (*MySQL, error) {
	id, err := run(ctx, "docker", "run", "-d", "--rm",
		"-e", "MYSQL_ROOT_PASSWORD="+mysqlPass,
		"-e", "MYSQL_DATABASE="+database,
		"-P", image,
	)
	if err != nil {
		return nil, fmt.Errorf("start mysql container: %w", err)
	}
	m := &MySQL{ID: strings.TrimSpace(id), Image: image, Database: database}
	if err := m.waitReady(ctx); err != nil {
		_ = m.Stop()
		return nil, err
	}
	return m, nil
}

func (m *MySQL) waitReady(ctx context.Context) error {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("mysql not ready before timeout: %w", ctx.Err())
		case <-ticker.C:
			// A real query against the target database, not mysqladmin ping: the
			// official image's entrypoint can report the server accepting TCP
			// connections before MYSQL_DATABASE is fully initialized. Querying "select
			// 1" against the real database is the signal we actually want, the same
			// reasoning StartPostgres uses for pg_isready.
			out, err := m.mysqlExec(ctx, "-N", "-B", "-e", "select 1")
			if err == nil && strings.TrimSpace(out) == "1" {
				return nil
			}
		}
	}
}

// Restore loads a logical .sql dump into the container. This is the only source
// kind MySQL supports in v1 (spec 0024): a physical/binlog restore is deferred.
func (m *MySQL) Restore(ctx context.Context, path string) (string, error) {
	const dest = "/tmp/salvage-dump.sql"
	if _, err := run(ctx, "docker", "cp", path, m.ID+":"+dest); err != nil {
		return "", fmt.Errorf("copy dump into container: %w", err)
	}
	// The mysql CLI reads its script from stdin; `docker exec sh -c "mysql ... <
	// file"` runs the redirect inside the container so no dump content crosses the
	// docker-cp/exec boundary as an argument.
	script := "mysql -h 127.0.0.1 -u " + mysqlUser + " " + shquote(m.Database) + " < " + shquote(dest)
	if _, err := m.shExec(ctx, script); err != nil {
		return "", fmt.Errorf("mysql restore: %w", err)
	}
	return "", nil
}

// Query runs a SQL statement expected to return a single scalar value. Satisfies
// checks.Queryer, so the existing "sql" check evaluator (internal/checks/sql.go)
// works against a MySQL target with zero new evaluator code.
func (m *MySQL) Query(ctx context.Context, sql string) (string, error) {
	out, err := m.mysqlExec(ctx, "-N", "-B", "-e", sql, m.Database)
	if err != nil {
		return "", err
	}
	// -N -B (skip-column-names, batch/tab-separated) yields the scalar on its own
	// line for a single-column, single-row result — the same shape psql -tAqc
	// gives Postgres's Query.
	return firstField(out), nil
}

// QueryRows runs a SQL query and returns one []string per result row, with
// cells in SELECT order — the multi-row analogue of Query, via the same
// in-container `mysql -N -B` discipline (spec 0028 R3; no Go MySQL driver, per
// spec 0024 R5). -B renders rows as tab-separated lines, so splitting on
// newlines then tabs recovers the cells; a NULL cell arrives as the literal
// string "NULL" (batch-mode rendering). It powers the MySQL engine's scaffold
// discovery (internal/engine/mysql).
func (m *MySQL) QueryRows(ctx context.Context, sql string) ([][]string, error) {
	out, err := m.mysqlExec(ctx, "-N", "-B", "-e", sql, m.Database)
	if err != nil {
		return nil, err
	}
	var rows [][]string
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if line == "" {
			continue
		}
		rows = append(rows, strings.Split(line, "\t"))
	}
	return rows, nil
}

// firstField returns the first tab/newline-delimited field of out, trimmed.
func firstField(out string) string {
	out = strings.TrimSpace(out)
	if out == "" {
		return ""
	}
	line := strings.SplitN(out, "\n", 2)[0]
	return strings.SplitN(line, "\t", 2)[0]
}

// mysqlExec runs the mysql client inside the container over TCP (127.0.0.1, so it
// talks to the real server rather than a socket the image may not expose), with
// the root password forwarded via MYSQL_PWD (never as a -p command-line arg,
// which would be visible via `docker exec`/process listings).
func (m *MySQL) mysqlExec(ctx context.Context, args ...string) (string, error) {
	base := []string{"mysql", "-h", "127.0.0.1", "-u", mysqlUser}
	return m.exec(ctx, append(base, args...)...)
}

// shExec runs cmd via `sh -c` inside the container (for the input-redirect
// restore, which the mysql client cannot express as argv alone).
func (m *MySQL) shExec(ctx context.Context, cmd string) (string, error) {
	return m.exec(ctx, "sh", "-c", cmd)
}

// exec runs a command inside the container with the DB password in the env.
func (m *MySQL) exec(ctx context.Context, args ...string) (string, error) {
	full := append([]string{"exec", "-e", "MYSQL_PWD=" + mysqlPass, m.ID}, args...)
	return run(ctx, "docker", full...)
}

// Stop removes the container. Safe to call more than once.
func (m *MySQL) Stop() error {
	if m == nil || m.ID == "" {
		return nil
	}
	// Fresh context so teardown still runs if the parent ctx timed out.
	_, err := run(context.Background(), "docker", "kill", m.ID)
	return err
}
