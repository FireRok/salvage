package ephemeral

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"salvage.sh/internal/probe"
)

// resticRestoreDir is where every restic snapshot is restored inside the
// throwaway container. Checks probe files under this fixed root.
const resticRestoreDir = "/restore"

// Restic is a throwaway container used to restore and validate a restic
// filesystem backup. Like the pgBackRest path, no host restic is needed: the
// restic/restic image idles ("sleep infinity"), `restic restore` populates
// /restore, then file/command checks run against that tree via `docker exec`.
// The embedded probe.HostHTTP additionally carries the http check kind
// (backlog S4): requests run from the Salvage host, so a service brought up
// from the restored tree can be probed without touching the container's
// post-restore network isolation.
type Restic struct {
	probe.HostHTTP
	ID string
}

// StartRestic launches the (idle) restic restore container.
//
// repoVolume, when set, is mounted at repoPath (for a local-filesystem repo).
// For a remote repo (S3/B2/Azure/…) leave it empty and forward the backend
// credentials via passEnv. repository, when non-empty, is a non-secret repo
// location set as RESTIC_REPOSITORY; a secret repo/password is instead forwarded
// by name via passEnv. passEnv names environment variables to forward from
// Salvage's own process into the container *by name* — so secret values (restic
// password, cloud keys) never appear in command arguments (spec 0003).
func StartRestic(ctx context.Context, image, repository, repoVolume, repoPath string, passEnv []string) (*Restic, error) {
	args := []string{"run", "-d", "--rm"}
	if repoVolume != "" {
		// A local-filesystem repo mounts the volume where the repository points.
		// repoPath defaults to the repository (a local path like /repo) in
		// config.applyDefaults, so a bare volume name never reaches here.
		mountAt := repoPath
		if mountAt == "" {
			mountAt = repository
		}
		args = append(args, "-v", repoVolume+":"+mountAt)
	}
	if repository != "" {
		// A plain path/URL is not a secret, so passing it as a value is fine and
		// keeps the common local-repo case config-free. A secret repo is forwarded
		// by name via passEnv instead (RESTIC_REPOSITORY in the list below).
		args = append(args, "-e", "RESTIC_REPOSITORY="+repository)
	}
	for _, name := range passEnv {
		args = append(args, "-e", name) // value forwarded from Salvage's env, not args
	}
	// The restic/restic image's entrypoint is `restic`; override it so the
	// container idles and we can `docker exec restic …` repeatedly.
	args = append(args, "--entrypoint", "sh", image, "-c", "sleep infinity")
	id, err := run(ctx, "docker", args...)
	if err != nil {
		return nil, fmt.Errorf("start restic container: %w", err)
	}
	return &Restic{ID: strings.TrimSpace(id)}, nil
}

// Restore runs `restic restore <snapshot> --target /restore`. A failure here is
// the verdict: the backup did not come back.
//
// After a successful restore the container is dropped off every Docker network
// (spec 0003 R2): the fetch needs egress (a remote repo is downloaded), but the
// file/command checks that follow must run against an isolated tree — a restored
// `command` check could otherwise reach out. If isolation fails we abort rather
// than run checks against a connected container.
func (r *Restic) Restore(ctx context.Context, snapshot string) error {
	if snapshot == "" {
		snapshot = "latest"
	}
	if out, err := r.exec(ctx, "restic", "restore", snapshot, "--target", resticRestoreDir); err != nil {
		if detail := resticError(out); detail != "" {
			return fmt.Errorf("restic restore: %s", detail)
		}
		return fmt.Errorf("restic restore: %w", err)
	}
	if err := r.isolateNetwork(ctx); err != nil {
		return fmt.Errorf("isolate restored files from network: %w", err)
	}
	return nil
}

// Exists reports whether path exists within the restored tree. path is joined
// under /restore, so configs name paths as they were backed up (e.g.
// "etc/app.conf"), not container-absolute.
func (r *Restic) Exists(ctx context.Context, path string) (bool, error) {
	// `test -e` exits 0 when present, 1 when absent — a clean two-way answer that
	// only errors on an operational problem (e.g. exec failure), never on "absent".
	_, exit, err := r.RunCommand(ctx, "test -e "+shquote(r.abs(path))+" && echo yes")
	if err != nil {
		return false, err
	}
	return exit == 0, nil
}

// Count returns the number of files matching pattern within the restored tree.
// pattern is a `find -path` glob interpreted relative to /restore (e.g.
// "data/*.parquet"); the count is `find <root> -path '<root>/pattern' | wc -l`.
func (r *Restic) Count(ctx context.Context, pattern string) (int, error) {
	root := resticRestoreDir
	globbed := root + "/" + strings.TrimLeft(pattern, "/")
	out, _, err := r.RunCommand(ctx,
		"find "+shquote(root)+" -path "+shquote(globbed)+" 2>/dev/null | wc -l")
	if err != nil {
		return 0, err
	}
	n, perr := strconv.Atoi(strings.TrimSpace(out))
	if perr != nil {
		return 0, fmt.Errorf("count: unexpected find output %q: %w", strings.TrimSpace(out), perr)
	}
	return n, nil
}

// Sha256 returns the lowercase hex sha256 of the file at path (under /restore).
func (r *Restic) Sha256(ctx context.Context, path string) (string, error) {
	out, exit, err := r.RunCommand(ctx, "sha256sum "+shquote(r.abs(path)))
	if err != nil {
		return "", err
	}
	if exit != 0 {
		return "", fmt.Errorf("sha256sum %q exited %d: %s", path, exit, strings.TrimSpace(out))
	}
	// sha256sum prints "<hex>  <path>"; keep the digest.
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return "", fmt.Errorf("sha256sum produced no output for %q", path)
	}
	return fields[0], nil
}

// RunCommand runs cmd via `sh -c` inside the restored tree (cwd /restore) and
// returns its stdout, exit code, and an error only for an operational failure
// (the exec itself failing). A non-zero exit is returned in exit, not as err, so
// a check can distinguish "command ran and failed" (a verdict) from "could not
// run the command" (operational).
func (r *Restic) RunCommand(ctx context.Context, cmd string) (string, int, error) {
	// Wrap so we always capture the exit code even when cmd exits non-zero, and
	// cd into /restore so relative paths in a user command resolve there.
	wrapped := "cd " + shquote(resticRestoreDir) + " 2>/dev/null; " + cmd + "; printf '\\n__EXIT__%d' \"$?\""
	out, err := r.exec(ctx, "sh", "-c", wrapped)
	if err != nil {
		// exec failed for an operational reason (container gone, docker error). The
		// __EXIT__ marker below is only appended when sh actually ran, so its
		// absence here means the command never executed.
		if !strings.Contains(out, "__EXIT__") {
			return out, -1, err
		}
	}
	stdout, exit := splitExit(out)
	return stdout, exit, nil
}

// Stop removes the container. Safe to call more than once.
func (r *Restic) Stop() error {
	if r == nil || r.ID == "" {
		return nil
	}
	// Fresh context so teardown still runs if the parent ctx timed out.
	_, err := run(context.Background(), "docker", "kill", r.ID)
	return err
}

// abs joins a config-relative path under the restore root.
func (r *Restic) abs(path string) string {
	return resticRestoreDir + "/" + strings.TrimLeft(path, "/")
}

// isolateNetwork disconnects the container from every Docker network, so a
// `command` check running restored, attacker-controlled content cannot reach the
// network (spec 0003 R2). Mirrors the pgBackRest path; disconnecting nothing (a
// local-repo restore with no external networks) is a success.
func (r *Restic) isolateNetwork(ctx context.Context) error {
	out, err := run(ctx, "docker", "inspect", "-f",
		"{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{end}}", r.ID)
	if err != nil {
		return fmt.Errorf("inspect container networks: %w", err)
	}
	for _, net := range parseNetworkList(out) {
		if _, err := run(ctx, "docker", "network", "disconnect", net, r.ID); err != nil {
			return fmt.Errorf("disconnect network %q: %w", net, err)
		}
	}
	return nil
}

func (r *Restic) exec(ctx context.Context, args ...string) (string, error) {
	full := append([]string{"exec", r.ID}, args...)
	return run(ctx, "docker", full...)
}

// resticError extracts the first "Fatal:" or "error" line from restic's output so
// a failed restore's verdict reason is the real cause rather than a bare exit
// status. Returns "" if no such line is present.
func resticError(out string) string {
	for _, ln := range strings.Split(out, "\n") {
		l := strings.TrimSpace(ln)
		low := strings.ToLower(l)
		if strings.HasPrefix(low, "fatal:") || strings.HasPrefix(low, "error:") ||
			strings.Contains(low, "unable to open") {
			return l
		}
	}
	return ""
}

// splitExit separates the captured stdout from the "__EXIT__<n>" marker appended
// by RunCommand, returning the stdout (trailing newline trimmed) and exit code.
// A missing/garbled marker yields exit 0 with the raw output — the command
// produced output but the wrapper somehow lost the code; treat it as success and
// let the check judge the output.
func splitExit(out string) (string, int) {
	i := strings.LastIndex(out, "__EXIT__")
	if i < 0 {
		return strings.TrimRight(out, "\n"), 0
	}
	code, err := strconv.Atoi(strings.TrimSpace(out[i+len("__EXIT__"):]))
	if err != nil {
		return strings.TrimRight(out[:i], "\n"), 0
	}
	return strings.TrimRight(out[:i], "\n"), code
}

// shquote single-quotes s for safe embedding in an `sh -c` string, escaping any
// embedded single quotes. Used for paths/globs we build; user commands are passed
// through verbatim (the command kind is explicitly "run this in the tree").
func shquote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
