# borg engine — end-to-end verification (spec 0022)

Closes the verification debt recorded in `specs/BACKLOG.md` section C: spec 0022
was implemented without a live Docker daemon; this records the first real run.

- **Date:** 2026-07-01
- **Docker:** server 29.6.1 (Docker Desktop, macOS/arm64)
- **Image:** `ghcr.io/borgmatic-collective/borgmatic:latest`
  (`sha256:0e07cc97c4de34054bd7f562037762aa95509b310cd13ea98db3e6e5402d9e21`)
  — borg 1.4.4. **Not** the image the spec/example config name; see finding.
- **Backup:** `./dev/borg/make-backup.sh` (fixed, see below) — seeds 4 known
  files into `salvage-borg-seed`, `borg init --encryption=repokey-blake2` in
  `salvage-borg-repo`, one archive `seed`.
- **Command:**
  `BORG_PASSPHRASE=salvage-dev salvage run -config salvage.borg.example.yaml`
  (run against a copy of the example config with `restore.image` swapped to the
  borgmatic image and `report.out` redirected out of the repo root; everything
  else verbatim).

## Verdict

```
salvage: target "demo-borg"
  restore   ok    (589ms)
  check     ok    config_present
  check     ok    data_files_present         3 within bounds
  check     ok    seed_checksum
  check     ok    config_readable
  verdict   PASS        (exit 0)
```

Negative path (spec 0022 acceptance): corrupting the `seed_checksum`
expectation flips exactly that check to FAIL with got/want shown; verdict FAIL,
exit 1. Confirmed.

## Finding: `ghcr.io/borgbackup/borg:stable` does not exist

The image named by `salvage.borg.example.yaml` and spec 0022 is not pullable —
GHCR refuses even an anonymous pull token for `borgbackup/borg` (the borg
project publishes no official Docker image). Fixes applied/needed:

- `dev/borg/make-backup.sh` (this dir, fixed 2026-07-01): defaults to the
  borgmatic-collective image and pins `--entrypoint borg` so any image with
  borg on PATH works.
- `salvage.borg.example.yaml` `restore.image` and the spec 0022 examples still
  reference the nonexistent image — owned elsewhere, needs the same swap. The
  engine itself is agnostic (it overrides the entrypoint with `sh` and calls
  `borg` from PATH), so only the documented default is wrong, not the code.
