package ephemeral

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"salvage.sh/internal/probe"
)

// borgRestoreDir is where every borg archive is extracted inside the throwaway
// container. Checks probe files under this fixed root. borg extract writes into
// the current working directory, so the exec into the container runs `cd` here
// first; see Restore.
const borgRestoreDir = "/restore"

// Borg is a throwaway container used to extract and validate a BorgBackup
// filesystem archive. Like the restic path, no host borg is needed: the
// borgbackup image idles ("sleep infinity"), `borg extract` populates /restore,
// then file/command checks run against that tree via `docker exec`.
//
// It is a near-exact sibling of Restic: only the tool invocation (borg extract
// into a cwd vs. restic restore --target) and the error extractor differ; the
// prober (Exists/Count/Sha256/RunCommand), the fixed restore root, and the
// two-phase network isolation are identical.
type Borg struct {
	// HostHTTP carries the http check kind (backlog S4), exactly as on Restic:
	// requests run from the Salvage host, not the isolated container.
	probe.HostHTTP
	ID string
}

// StartBorg launches the (idle) borg extract container.
//
// repoVolume, when set, is mounted at repoPath (for a local-filesystem repo).
// For a remote repo (ssh://…) leave it empty and forward the credentials via
// passEnv. repository, when non-empty, is a non-secret repo location set as
// BORG_REPO; a secret repo/passphrase is instead forwarded by name via passEnv
// (BORG_REPO, BORG_PASSPHRASE, …). passEnv names environment variables to
// forward from Salvage's own process into the container *by name* — so secret
// values (the borg passphrase, ssh keys) never appear in command arguments
// (spec 0003).
func StartBorg(ctx context.Context, image, repository, repoVolume, repoPath string, passEnv []string) (*Borg, error) {
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
		// by name via passEnv instead (BORG_REPO in the list below).
		args = append(args, "-e", "BORG_REPO="+repository)
	}
	for _, name := range passEnv {
		args = append(args, "-e", name) // value forwarded from Salvage's env, not args
	}
	// The borgbackup image's entrypoint is `borg`; override it so the container
	// idles and we can `docker exec borg …` repeatedly. The restore dir is
	// created up front so `borg extract` (which writes into cwd) has somewhere to
	// land.
	args = append(args, "--entrypoint", "sh", image, "-c",
		"mkdir -p "+borgRestoreDir+" && sleep infinity")
	id, err := run(ctx, "docker", args...)
	if err != nil {
		return nil, fmt.Errorf("start borg container: %w", err)
	}
	return &Borg{ID: strings.TrimSpace(id)}, nil
}

// Restore runs `borg extract ::<archive>` from the restore directory. A failure
// here is the verdict: the backup did not come back.
//
// borg extract writes into the current working directory (there is no --target
// flag), so it is run with cwd /restore. The archive is addressed as
// `<repo>::<archive>`; with BORG_REPO set the shorthand `::<archive>` selects
// that repo, so the repository never appears on the command line.
//
// After a successful extract the container is dropped off every Docker network
// (spec 0003 R2): the fetch may need egress (a remote ssh:// repo is
// downloaded), but the file/command checks that follow must run against an
// isolated tree — a restored `command` check could otherwise reach out. If
// isolation fails we abort rather than run checks against a connected container.
func (b *Borg) Restore(ctx context.Context, archive string) error {
	if archive == "" {
		return fmt.Errorf("borg extract: archive is required (set source.archive)")
	}
	// cd into the restore dir so extract lands under /restore, then extract the
	// archive by its short `::name` form (BORG_REPO supplies the repository).
	extract := "cd " + shquote(borgRestoreDir) + " && borg extract " + shquote("::"+archive)
	if out, err := b.exec(ctx, "sh", "-c", extract); err != nil {
		if detail := borgError(out); detail != "" {
			return fmt.Errorf("borg extract: %s", detail)
		}
		return fmt.Errorf("borg extract: %w", err)
	}
	if err := b.isolateNetwork(ctx); err != nil {
		return fmt.Errorf("isolate restored files from network: %w", err)
	}
	return nil
}

// Exists reports whether path exists within the restored tree. path is joined
// under /restore, so configs name paths as they were backed up (e.g.
// "etc/app.conf"), not container-absolute.
func (b *Borg) Exists(ctx context.Context, path string) (bool, error) {
	_, exit, err := b.RunCommand(ctx, "test -e "+shquote(b.abs(path))+" && echo yes")
	if err != nil {
		return false, err
	}
	return exit == 0, nil
}

// Count returns the number of files matching pattern within the restored tree.
// pattern is a `find -path` glob interpreted relative to /restore (e.g.
// "data/*.parquet"); the count is `find <root> -path '<root>/pattern' | wc -l`.
func (b *Borg) Count(ctx context.Context, pattern string) (int, error) {
	root := borgRestoreDir
	globbed := root + "/" + strings.TrimLeft(pattern, "/")
	out, _, err := b.RunCommand(ctx,
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
func (b *Borg) Sha256(ctx context.Context, path string) (string, error) {
	out, exit, err := b.RunCommand(ctx, "sha256sum "+shquote(b.abs(path)))
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
func (b *Borg) RunCommand(ctx context.Context, cmd string) (string, int, error) {
	wrapped := "cd " + shquote(borgRestoreDir) + " 2>/dev/null; " + cmd + "; printf '\\n__EXIT__%d' \"$?\""
	out, err := b.exec(ctx, "sh", "-c", wrapped)
	if err != nil {
		if !strings.Contains(out, "__EXIT__") {
			return out, -1, err
		}
	}
	stdout, exit := splitExit(out)
	return stdout, exit, nil
}

// Stop removes the container. Safe to call more than once.
func (b *Borg) Stop() error {
	if b == nil || b.ID == "" {
		return nil
	}
	// Fresh context so teardown still runs if the parent ctx timed out.
	_, err := run(context.Background(), "docker", "kill", b.ID)
	return err
}

// abs joins a config-relative path under the restore root.
func (b *Borg) abs(path string) string {
	return borgRestoreDir + "/" + strings.TrimLeft(path, "/")
}

// isolateNetwork disconnects the container from every Docker network, so a
// `command` check running restored, attacker-controlled content cannot reach the
// network (spec 0003 R2). Mirrors the restic path; disconnecting nothing (a
// local-repo restore with no external networks) is a success.
func (b *Borg) isolateNetwork(ctx context.Context) error {
	out, err := run(ctx, "docker", "inspect", "-f",
		"{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{end}}", b.ID)
	if err != nil {
		return fmt.Errorf("inspect container networks: %w", err)
	}
	for _, net := range parseNetworkList(out) {
		if _, err := run(ctx, "docker", "network", "disconnect", net, b.ID); err != nil {
			return fmt.Errorf("disconnect network %q: %w", net, err)
		}
	}
	return nil
}

func (b *Borg) exec(ctx context.Context, args ...string) (string, error) {
	full := append([]string{"exec", b.ID}, args...)
	return run(ctx, "docker", full...)
}

// borgError extracts the first error line from borg's output so a failed
// extract's verdict reason is the real cause rather than a bare exit status.
// borg logs like "Repository … does not exist." or lines prefixed with an ERROR
// level; returns "" if no such line is present.
func borgError(out string) string {
	for _, ln := range strings.Split(out, "\n") {
		l := strings.TrimSpace(ln)
		low := strings.ToLower(l)
		if strings.HasPrefix(low, "error:") || strings.HasPrefix(low, "error ") ||
			strings.Contains(low, "passphrase supplied") ||
			strings.Contains(low, "does not exist") ||
			strings.Contains(low, "does not seem to be a valid repository") {
			return l
		}
	}
	return ""
}
