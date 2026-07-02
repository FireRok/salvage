# restic engine — end-to-end verification (spec 0018)

Closes the verification debt recorded in `specs/BACKLOG.md` section C: spec 0018
was implemented without a live Docker daemon; this records the first real run.

- **Date:** 2026-07-01
- **Docker:** server 29.6.1 (Docker Desktop, macOS/arm64)
- **Image:** `restic/restic:latest`
  (`sha256:66591595b31c2874386924edc80be55f21456d603ac9bf18b4384bd6236c843b`)
  — restic 0.19.0, linux/arm64
- **Backup:** `./dev/restic/make-backup.sh` (unchanged) — seeds 4 known files
  into the `salvage-restic-seed` volume, inits a repo in `salvage-restic-repo`,
  one snapshot of `/seed`.
- **Command:**
  `RESTIC_PASSWORD=salvage-dev salvage run -config salvage.restic.example.yaml`
  (run against a copy of the example config with only `report.out` redirected
  out of the repo root; all target/source/check stanzas verbatim).

## Verdict

```
salvage: target "demo-restic"
  restore   ok    (1023ms)
  check     ok    config_present
  check     ok    data_files_present         3 within bounds
  check     ok    seed_checksum
  check     ok    config_readable
  verdict   PASS        (exit 0)
```

Negative path (spec 0018 acceptance 2): corrupting the `seed_checksum`
expectation flips exactly that check to FAIL with got/want shown; verdict FAIL,
exit 1. Confirmed.

No engine bugs found.
