package ephemeral

import (
	"context"
	"fmt"
	"strings"
)

const pgData = "/var/lib/postgresql/data"

// PgBackRest is a throwaway container used to restore and validate a pgBackRest
// physical backup. Unlike the logical path, no initdb happens: the container
// idles, `pgbackrest restore` populates PGDATA, then Postgres is started on it
// and replays WAL until it reaches a consistent, connectable state.
type PgBackRest struct {
	ID       string
	Database string
	User     string
	// PreloadLibraries seeds shared_preload_libraries in a synthesized
	// postgresql.conf when the restored PGDATA lacks one (config-outside-PGDATA
	// clusters, e.g. Debian-packaged Postgres).
	PreloadLibraries []string
}

// StartRestoreEnv launches the (idle) restore container.
//
// repoVolume, when set, is mounted at repoPath (for a local-filesystem repo).
// For a remote repo (S3/R2) leave it empty and rely on the image's
// pgbackrest.conf. passEnv names environment variables to forward from Salvage's
// own process into the container *by name* — so secret values (e.g. R2 keys)
// never appear in command arguments. preloadLibs seeds a synthesized config (see
// PreloadLibraries).
func StartRestoreEnv(ctx context.Context, image, repoPath, repoVolume, database, user string, passEnv, preloadLibs []string) (*PgBackRest, error) {
	if database == "" {
		database = "postgres"
	}
	if user == "" {
		user = "postgres"
	}
	args := []string{"run", "-d", "--rm", "--shm-size=256m"}
	if repoVolume != "" {
		args = append(args, "-v", repoVolume+":"+repoPath)
	}
	for _, name := range passEnv {
		args = append(args, "-e", name) // value forwarded from Salvage's env, not args
	}
	// "sleep infinity" makes the postgres entrypoint exec the command directly
	// instead of running initdb, so PGDATA stays empty for the restore.
	args = append(args, image, "sleep", "infinity")
	id, err := run(ctx, "docker", args...)
	if err != nil {
		return nil, fmt.Errorf("start restore container: %w", err)
	}
	return &PgBackRest{ID: strings.TrimSpace(id), Database: database, User: user, PreloadLibraries: preloadLibs}, nil
}

// Restore runs `pgbackrest restore`, then starts Postgres and waits for it to
// finish recovery and accept connections — the moment it has reached a
// consistent recovery point. A failure here is the verdict: the backup did not
// come back.
func (p *PgBackRest) Restore(ctx context.Context, stanza, set string) error {
	// --type=immediate recovers to the backup's own consistency point (fast, and
	// the right "does this backup restore?" semantic) rather than replaying the
	// entire WAL archive to "latest". A non-empty set pins a specific backup
	// (--set=<label>) instead of the latest — used by last-known-good search.
	//
	// --pg1-path pins the restore TARGET to the ephemeral container's PGDATA. We
	// always restore into the same throwaway path, so passing it explicitly frees
	// the image's pgbackrest.conf from needing a pg1-path per stanza — it only
	// needs the [global] repo config. This is what makes a fleet of stanzas
	// restorable through one generic image.
	args := []string{"pgbackrest", "--stanza=" + stanza, "--pg1-path=" + pgData, "--type=immediate"}
	if set != "" {
		args = append(args, "--set="+set)
	}
	args = append(args, "restore")
	// pgBackRest writes its console log — including the ERROR: line — to stdout, so
	// the stderr captured by run() is empty on failure. Pull the error out of the
	// console output so the verdict reason is the real cause (e.g. a missing file)
	// rather than a bare "exit status 55".
	if out, err := p.exec(ctx, args...); err != nil {
		if detail := pgbackrestError(out); detail != "" {
			return fmt.Errorf("pgbackrest restore: %s", detail)
		}
		return fmt.Errorf("pgbackrest restore: %w", err)
	}
	// Debian-packaged Postgres keeps postgresql.conf / pg_hba.conf / pg_ident.conf in
	// /etc/postgresql/<ver>/<cluster>/, OUTSIDE PGDATA — so a pgBackRest backup of the
	// data dir doesn't include them and the restored cluster can't start. Synthesize a
	// minimal postgresql.conf when one is missing.
	if err := p.ensureConfig(ctx); err != nil {
		return fmt.Errorf("synthesize postgresql config: %w", err)
	}
	// The restored cluster carries (or, on Debian, lacks) a pg_hba.conf. Append a
	// trust line — creating the file if absent — so the verifier can read the
	// throwaway copy regardless of the production auth config.
	if _, err := p.exec(ctx, "sh", "-c",
		"printf '\\nlocal all all trust\\n' >> "+pgData+"/pg_hba.conf"); err != nil {
		return fmt.Errorf("relax pg_hba for verification: %w", err)
	}
	// pg_ctl -w blocks until the server accepts connections, i.e. recovery has
	// reached consistency. archive_mode=off so the throwaway doesn't try to push
	// WAL back into the repo it was restored from.
	if _, err := p.exec(ctx, "pg_ctl", "-D", pgData, "-l", "/tmp/pg.log",
		"-w", "-t", "120", "-o", "-c archive_mode=off", "start"); err != nil {
		log, _ := p.exec(context.Background(), "tail", "-n", "40", "/tmp/pg.log")
		return fmt.Errorf("restored postgres did not reach a consistent state: %w\n--- server log ---\n%s", err, log)
	}
	// Spec 0003 R2 — two-phase network isolation. BOTH the restore fetch (a remote
	// S3/R2 repo is downloaded over the network) AND recovery (archive-get pulls WAL
	// to reach consistency) need egress, so the container stays connected through the
	// pg_ctl start above. Now that the cluster has reached a consistent state, drop it
	// off every Docker network BEFORE any check queries it: the restored production
	// data may carry pg_cron jobs or FDW/dblink targets that must never reach out.
	// If isolation fails we abort rather than run checks against a connected cluster.
	if err := p.isolateNetwork(ctx); err != nil {
		return fmt.Errorf("isolate restored cluster from network: %w", err)
	}
	return nil
}

// Info returns the raw `pgbackrest info --output=json`. With a non-empty stanza
// it scopes to that stanza (the backup chain for last-known-good search); with an
// empty stanza it omits --stanza so every stanza in the repo is reported (fleet
// discovery).
func (p *PgBackRest) Info(ctx context.Context, stanza string) ([]byte, error) {
	args := []string{"pgbackrest", "--output=json"}
	if stanza != "" {
		args = append(args, "--stanza="+stanza)
	}
	args = append(args, "info")
	out, err := p.exec(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("pgbackrest info: %w", err)
	}
	return []byte(out), nil
}

// ensureConfig handles clusters that keep their config outside PGDATA (e.g.
// Debian-packaged Postgres): if postgresql.conf is missing after restore it
// synthesizes a minimal one — shared_preload_libraries from PreloadLibraries,
// hot_standby on (so read-only checks work even at a paused recovery target), and
// the recovery-critical max_* settings copied from the control file so Postgres's
// "value must be >= primary" check passes. pg_ident.conf is created if absent. An
// existing postgresql.conf (clusters that keep config in PGDATA) is left untouched.
func (p *PgBackRest) ensureConfig(ctx context.Context) error {
	conf := pgData + "/postgresql.conf"
	if _, err := p.exec(ctx, "touch", pgData+"/pg_ident.conf"); err != nil {
		return fmt.Errorf("create pg_ident.conf: %w", err)
	}
	if _, err := p.exec(ctx, "test", "-f", conf); err == nil {
		return nil // cluster keeps config in PGDATA; leave it alone
	}
	cd, err := p.exec(ctx, "pg_controldata", pgData)
	if err != nil {
		return fmt.Errorf("read control file: %w", err)
	}
	lines := []string{
		"shared_preload_libraries = '" + strings.Join(p.PreloadLibraries, ",") + "'",
		"hot_standby = on",
	}
	for label, guc := range map[string]string{
		"max_connections setting:":      "max_connections",
		"max_worker_processes setting:": "max_worker_processes",
		"max_wal_senders setting:":      "max_wal_senders",
		"max_prepared_xacts setting:":   "max_prepared_transactions",
		"max_locks_per_xact setting:":   "max_locks_per_transaction",
	} {
		if v := controldataValue(cd, label); v != "" {
			lines = append(lines, guc+" = "+v)
		}
	}
	body := strings.Join(lines, "\n") + "\n"
	script := "cat > " + conf + " <<'SALVAGE_EOF'\n" + body + "SALVAGE_EOF\n"
	if _, err := p.exec(ctx, "sh", "-c", script); err != nil {
		return fmt.Errorf("write postgresql.conf: %w", err)
	}
	return nil
}

// pgbackrestError extracts the first "ERROR:" line from pgBackRest console output
// and returns it from "ERROR:" onward, with the leading timestamp/process prefix
// stripped. Returns "" if no error line is present.
func pgbackrestError(consoleOut string) string {
	for _, ln := range strings.Split(consoleOut, "\n") {
		if i := strings.Index(ln, "ERROR:"); i >= 0 {
			return strings.TrimSpace(ln[i:])
		}
	}
	return ""
}

// controldataValue returns the value after a "label" line in pg_controldata output.
func controldataValue(out, label string) string {
	for _, ln := range strings.Split(out, "\n") {
		if i := strings.Index(ln, label); i >= 0 {
			return strings.TrimSpace(ln[i+len(label):])
		}
	}
	return ""
}

// Query runs a scalar SQL statement against the restored database over the local
// socket (pg_hba was relaxed to trust local connections above).
func (p *PgBackRest) Query(ctx context.Context, sql string) (string, error) {
	return p.exec(ctx, "psql", "-U", p.User, "-d", p.Database, "-tAqc", sql)
}

// QueryRows runs a SQL statement and returns its rows, each a slice of column
// strings in SELECT order. Satisfies discover.RowQueryer (used by scaffold).
func (p *PgBackRest) QueryRows(ctx context.Context, sql string) ([][]string, error) {
	out, err := p.exec(ctx, "psql", "-U", p.User, "-d", p.Database, "-t", "-A", "-F", "\t", "-c", sql)
	if err != nil {
		return nil, err
	}
	return parseRows(out), nil
}

// Stop removes the container. Safe to call more than once.
func (p *PgBackRest) Stop() error {
	if p == nil || p.ID == "" {
		return nil
	}
	_, err := run(context.Background(), "docker", "kill", p.ID)
	return err
}

// isolateNetwork disconnects the container from every Docker network it is
// attached to, so the cluster started on restored production data (spec 0003 R2)
// cannot reach the network or be reached. Called after the cluster has reached a
// consistent state and before any check queries it.
//
// Local-repo restores may have no external networks; disconnecting nothing is a
// success (the "already no networks" case). If any required disconnect fails the
// caller must abort rather than query a connected cluster.
func (p *PgBackRest) isolateNetwork(ctx context.Context) error {
	out, err := run(ctx, "docker", "inspect", "-f",
		"{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{end}}", p.ID)
	if err != nil {
		return fmt.Errorf("inspect container networks: %w", err)
	}
	for _, net := range parseNetworkList(out) {
		if _, err := run(ctx, "docker", "network", "disconnect", net, p.ID); err != nil {
			return fmt.Errorf("disconnect network %q: %w", net, err)
		}
	}
	return nil
}

// parseNetworkList splits the space-separated network names emitted by the
// `docker inspect` Go template into a slice, dropping empty fields. Pure (no
// Docker dependency) so it is unit-testable.
func parseNetworkList(inspectOut string) []string {
	var nets []string
	for _, f := range strings.Fields(inspectOut) {
		if f != "" {
			nets = append(nets, f)
		}
	}
	return nets
}

func (p *PgBackRest) exec(ctx context.Context, args ...string) (string, error) {
	full := append([]string{"exec", "-u", "postgres", p.ID}, args...)
	return run(ctx, "docker", full...)
}
