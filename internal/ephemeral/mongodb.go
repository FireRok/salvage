package ephemeral

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	// mongoUser/mongoPass are Salvage's own fixed dev-only credentials for the
	// throwaway container, mirroring mysqlUser/mysqlPass and pgUser/pgPass. The
	// container is single-use and destroyed on Stop(); it never holds a customer
	// secret. source.pass_env is still honored/forwarded for forward
	// compatibility (see mongodb.requireEnv), even though v1 has no required var.
	mongoUser = "root"
	mongoPass = "salvage"
)

// MongoDB is a running throwaway MongoDB container. Like MySQL there is no Go
// driver dependency: every interaction shells to the mongosh CLI inside the
// container via `docker exec` (internal/engine/spi and spec 0016 R7 keep the
// module's only dependency gopkg.in/yaml.v3).
type MongoDB struct {
	ID       string
	Image    string
	Database string
}

// StartMongoDB launches a disposable MongoDB container (authenticated with
// Salvage's own fixed dev credentials) and waits for it to accept real queries.
// The container is started with --rm so Stop fully removes it.
func StartMongoDB(ctx context.Context, image, database string) (*MongoDB, error) {
	id, err := run(ctx, "docker", "run", "-d", "--rm",
		"-e", "MONGO_INITDB_ROOT_USERNAME="+mongoUser,
		"-e", "MONGO_INITDB_ROOT_PASSWORD="+mongoPass,
		"-P", image,
	)
	if err != nil {
		return nil, fmt.Errorf("start mongodb container: %w", err)
	}
	m := &MongoDB{ID: strings.TrimSpace(id), Image: image, Database: database}
	if err := m.waitReady(ctx); err != nil {
		_ = m.Stop()
		return nil, err
	}
	return m, nil
}

func (m *MongoDB) waitReady(ctx context.Context) error {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("mongodb not ready before timeout: %w", ctx.Err())
		case <-ticker.C:
			// A real query via mongosh, not a bare TCP/port probe: the official
			// image's entrypoint can accept connections before the auth database
			// is fully initialized — the same reasoning StartPostgres/StartMySQL
			// use for pg_isready/mysqladmin ping.
			out, err := m.mongoshEval(ctx, "db.runCommand({ping:1}).ok")
			if err == nil && strings.TrimSpace(out) == "1" {
				return nil
			}
		}
	}
}

// Restore loads a mongodump --archive file into the container. This is the only
// source kind MongoDB supports in v1: a physical/oplog restore is deferred (see
// spec 0025 Open questions).
func (m *MongoDB) Restore(ctx context.Context, path string) (string, error) {
	const dest = "/tmp/salvage-dump.archive"
	if _, err := run(ctx, "docker", "cp", path, m.ID+":"+dest); err != nil {
		return "", fmt.Errorf("copy archive into container: %w", err)
	}
	// mongorestore reads its own --archive flag; no shell redirect is needed (the
	// archive is a self-contained mongodump format, not a script piped to a
	// client), but we still run it via docker exec with an explicit argv so the
	// archive content never crosses the boundary as a raw argument — only the
	// in-container path does. --nsInclude scopes the restore to the configured
	// database; the archive is expected to have been produced by `mongodump
	// --db <database>` (or an equivalent single-database dump), consistent with
	// target.restore.database naming the database checks connect to.
	args := []string{
		"mongorestore",
		"--username", mongoUser,
		"--authenticationDatabase", "admin",
		"--archive=" + dest,
		"--nsInclude", m.Database + ".*",
		"--drop",
	}
	if _, err := m.exec(ctx, args...); err != nil {
		return "", fmt.Errorf("mongorestore: %w", err)
	}
	return "", nil
}

// CountDocuments returns the number of documents in collection matching
// filterJSON (a JSON filter document; "" or "{}" counts every document).
// Satisfies mongodb.MongoQueryer.
func (m *MongoDB) CountDocuments(ctx context.Context, collection, filterJSON string) (int64, error) {
	filter := filterJSON
	if strings.TrimSpace(filter) == "" {
		filter = "{}"
	}
	script := fmt.Sprintf("db.getCollection(%s).countDocuments(%s)", jsQuote(collection), filter)
	out, err := m.mongoshEval(ctx, script)
	if err != nil {
		return 0, err
	}
	n, perr := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if perr != nil {
		return 0, fmt.Errorf("countDocuments: unexpected output %q: %w", out, perr)
	}
	return n, nil
}

// FindOneField runs findOne(filterJSON) against collection and returns the
// dotted field's value, rendered as a scalar string. Satisfies
// mongodb.MongoQueryer.
func (m *MongoDB) FindOneField(ctx context.Context, collection, filterJSON, field string) (string, error) {
	filter := filterJSON
	if strings.TrimSpace(filter) == "" {
		filter = "{}"
	}
	// tojson(undefined) is "undefined" in mongosh, giving a clear "field missing
	// or no matching document" signal rather than a crash on a nil dereference.
	script := fmt.Sprintf(
		`(() => { const d = db.getCollection(%s).findOne(%s); if (d === null) return "__salvage_no_match__"; const v = %s; return (v === undefined) ? "__salvage_no_field__" : (v instanceof Date ? v.toISOString() : String(v)); })()`,
		jsQuote(collection), filter, fieldAccessExpr("d", field),
	)
	out, err := m.mongoshEval(ctx, script)
	if err != nil {
		return "", err
	}
	out = strings.TrimSpace(out)
	switch out {
	case "__salvage_no_match__":
		return "", fmt.Errorf("doc_query: no document matched filter %s", filter)
	case "__salvage_no_field__":
		return "", fmt.Errorf("doc_query: field %q not present on matched document", field)
	}
	return out, nil
}

// fieldAccessExpr builds a safe optional-chained JS field access (e.g.
// `d?.meta?.version`) from a dotted field path, so a missing intermediate
// object yields undefined instead of throwing inside the eval script.
func fieldAccessExpr(root, field string) string {
	expr := root
	for _, part := range strings.Split(field, ".") {
		if part == "" {
			continue
		}
		expr += "?.[" + jsQuote(part) + "]"
	}
	return expr
}

// jsQuote renders s as a double-quoted JS string literal for embedding in a
// mongosh --eval script.
func jsQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// mongoshEval runs script via `mongosh --quiet --eval` and returns its trimmed
// stdout — the scalar result of the evaluated expression.
func (m *MongoDB) mongoshEval(ctx context.Context, script string) (string, error) {
	out, err := m.exec(ctx, "mongosh",
		"--quiet",
		"--username", mongoUser,
		"--authenticationDatabase", "admin",
		m.Database,
		"--eval", script,
	)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// exec runs args (mongosh/mongorestore and their flags) inside the container.
// Unlike mysql's MYSQL_PWD, neither mongosh nor mongorestore has a universal
// password-via-environment-variable flag, so the password is instead supplied
// as a `--password "$MONGO_PWD"` shell expansion: the literal argument the
// outer `docker exec` process sees is the string "$MONGO_PWD", not the secret
// itself, and the shell inside the container performs the substitution from an
// env var scoped to that single exec — mirroring the by-reference discipline
// MYSQL_PWD/PGPASSWORD give the sibling engines, adapted to a client with no
// direct env-var password flag.
func (m *MongoDB) exec(ctx context.Context, args ...string) (string, error) {
	return run(ctx, "docker", buildExecArgs(m.ID, args)...)
}

// buildExecArgs builds the `docker exec` argv for running args (mongosh/
// mongorestore and their flags) inside container id, with the password
// supplied via the "$MONGO_PWD" shell-expansion placeholder described on exec.
// Factored out as a pure function so the argv shape can be unit-tested without
// running Docker.
func buildExecArgs(id string, args []string) []string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = shquote(a)
	}
	script := strings.Join(quoted, " ") + ` --password "$MONGO_PWD"`
	return []string{"exec", "-e", "MONGO_PWD=" + mongoPass, id, "sh", "-c", script}
}

// Stop removes the container. Safe to call more than once.
func (m *MongoDB) Stop() error {
	if m == nil || m.ID == "" {
		return nil
	}
	// Fresh context so teardown still runs if the parent ctx timed out.
	_, err := run(context.Background(), "docker", "kill", m.ID)
	return err
}
