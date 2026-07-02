#!/usr/bin/env bash
# Reproduce a local backup artifact for exercising Salvage's exec engine
# (spec 0020, bring-your-own-restore). Seeds a SQLite "application" database
# with known data (10 orders rows, 2 customers), snapshots it with sqlite3's
# .backup, and packages the backup + a deterministic MANIFEST into
# dev/exec/work/backup.tar.gz — the artifact the customer's own restore script
# (./restore.sh) restores. Needs only sqlite3 and tar (preinstalled on macOS,
# ubiquitous on Linux) — no Docker. Then, from the repo root:
#
#   salvage run -config dev/exec/salvage.exec.demo.yaml
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
WORK="$HERE/work"
DB="$WORK/seed/app.db"

mkdir -p "$WORK/seed" "$WORK/stage"

echo ">> seed sqlite database $DB"
# DROP-then-CREATE keeps re-runs idempotent without deleting any files.
sqlite3 "$DB" <<'SQL'
DROP TABLE IF EXISTS orders;
DROP TABLE IF EXISTS customers;
DROP TABLE IF EXISTS schema_migrations;
CREATE TABLE customers (id INTEGER PRIMARY KEY, name TEXT NOT NULL);
INSERT INTO customers (id, name) VALUES (1, 'alice'), (2, 'bob');
CREATE TABLE orders (
  id INTEGER PRIMARY KEY,
  customer_id INTEGER NOT NULL REFERENCES customers(id),
  amount_cents INTEGER NOT NULL
);
INSERT INTO orders (customer_id, amount_cents) VALUES
  (1, 1000), (1, 2000), (1, 3000), (1, 4000), (1, 5000),
  (2, 6000), (2, 7000), (2, 8000), (2, 9000), (2, 10000);
CREATE TABLE schema_migrations (version TEXT PRIMARY KEY);
INSERT INTO schema_migrations VALUES ('20260701000000');
SQL

echo ">> sqlite3 .backup -> $WORK/stage/app.db.backup"
sqlite3 "$DB" ".backup '$WORK/stage/app.db.backup'"

# Deterministic MANIFEST: fixed bytes -> fixed sha256, asserted by the demo
# config's checksum check. Do not change without updating the config.
printf 'salvage-exec-demo\nformat: sqlite3-backup\ntables: customers,orders,schema_migrations\norders_rows: 10\n' \
  > "$WORK/stage/MANIFEST"

echo ">> package artifact $WORK/backup.tar.gz"
tar -czf "$WORK/backup.tar.gz" -C "$WORK/stage" app.db.backup MANIFEST

cat <<EOF

>> done — artifact at dev/exec/work/backup.tar.gz (10 orders rows).
   ./restore.sh (run by Salvage) restores it into dev/exec/restored/.
   Run, from the repo root:
     salvage run -config dev/exec/salvage.exec.demo.yaml
EOF
