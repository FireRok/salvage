# exec engine — end-to-end verification (spec 0020)

First live run of the exec (bring-your-own-restore) engine against a real
restore command and a real backup artifact, via the new `dev/exec/` harness.
Docker-free by design: the demo needs only `sqlite3` and `tar`.

- **Date:** 2026-07-02
- **Host:** macOS 26.5.1 (arm64), sqlite3 3.51.0, bsdtar 3.5.3
- **Backup:** `./dev/exec/make-backup.sh` — seeds a SQLite database with known
  data (10 `orders` rows, 2 `customers`, `schema_migrations`), snapshots it
  with `sqlite3 .backup`, and packages backup + deterministic MANIFEST into
  `dev/exec/work/backup.tar.gz` (gitignored). Idempotent; no Docker.
- **Restore:** `./dev/exec/restore.sh` — the "customer's own restore script"
  Salvage runs as `restore.command`: extracts the artifact into
  `dev/exec/restored/` (gitignored), installs `restored/app.db`, and gates
  exit 0 on `PRAGMA integrity_check`.
- **Command:** `salvage run -config dev/exec/salvage.exec.demo.yaml`
  (from the repo root; config verbatim — checks exercise `file_exists`,
  `file_count`, `checksum`, and two sqlite3-querying `command` kinds).

## Verdict

```
salvage: target "demo-exec"
  restore   ok    (20ms)
  restore   warn  restored …/dev/exec/work/backup.tar.gz -> …/dev/exec/restored/app.db [sha256:24bdf046…]
  check     ok    database_restored
  check     ok    restore_complete           2 within bounds
  check     ok    manifest_checksum
  check     ok    orders_row_count
  check     ok    known_customer_present
  verdict   PASS        (exit 0)
```

(The `restore warn` line is the restore command's stdout tail landing in the
report's restore detail, per spec 0020 R1 — expected, not a problem.)

Negative paths (spec 0020 acceptance 2), both confirmed:

- **Broken expectation:** editing the `orders_row_count` check to expect 9999
  rows flips exactly that check to FAIL (`got=exit 1`); verdict FAIL, exit 1.
- **Corrupt artifact:** overwriting `work/backup.tar.gz` with garbage makes
  the restore command exit non-zero — reported as
  `restore FAIL restore command exited non-zero: tar: Error opening archive:
  Unrecognized archive format`; verdict FAIL, exit 1 (a fail verdict, not an
  operational error — the operational-vs-verdict split held).

Re-running `make-backup.sh` after the corruption rebuilt the fixture and the
next run was PASS again (harness idempotency confirmed).

No engine bugs found.
