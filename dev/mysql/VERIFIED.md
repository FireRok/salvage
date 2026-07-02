# mysql engine — end-to-end verification (spec 0024)

Closes the verification debt recorded in `specs/BACKLOG.md` section C: spec 0024
was implemented without a live Docker daemon; this records the first real run.

- **Date:** 2026-07-01
- **Docker:** server 29.6.1 (Docker Desktop, macOS/arm64)
- **Image:** `mysql:8`
  (`sha256:d36d39a64cd12a5c1cc9e6aa2bfb5f8d4c81a2f6586e0a04a9ae13939db02209`)
  — MySQL Server 8.4.10, linux/arm64 (both seed and restore containers)
- **Backup:** `./dev/mysql/make-backup.sh` (fixed, see below) — seeds `seeddb`
  with 10 `orders` rows + `schema_migrations`, `mysqldump --databases seeddb
  --add-drop-database` to `dev/mysql/seed-dump.sql` (~3.2 KB, 2 CREATE TABLEs).
- **Command:** `salvage run -config salvage.mysql.example.yaml`
  (run against a copy of the example config with only `report.out` redirected
  out of the repo root and the dump path made absolute; checks verbatim).

## Verdict

```
salvage: target "demo-mysql"
  restore   ok    (5364ms)
  check     ok    orders_not_empty           10 within bounds
  check     ok    latest_order_recent        age 13s (max 48h0m0s)
  verdict   PASS        (exit 0)
```

Negative path (spec 0024 acceptance 2): raising `expect_min` to 1000 flips
`orders_not_empty` to FAIL (`got=10 10 < min 1000`); verdict FAIL, exit 1.
Confirmed.

## Finding: seed script heredoc was silently discarded (fixed here)

The original `make-backup.sh` seeded via `docker exec … mysql … <<'SQL'`
**without `-i`**, so stdin was never attached: mysql executed nothing, exited
0, and mysqldump produced a 1.5 KB dump with zero tables. Salvage then restored
that empty-but-valid dump "ok" and both checks errored with
`ERROR 1146 … Table 'seeddb.orders' doesn't exist` (verdict FAIL — correct
behavior for an empty backup, wrong fixture). Fixed 2026-07-01 by adding `-i`
to the docker exec. Engine code itself is correct; no Go changes needed.
