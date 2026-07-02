#!/usr/bin/env bash
# The customer's own restore script — what Salvage's exec engine runs as
# restore.command (spec 0020). Stands in for whatever bespoke procedure a real
# operator has: it takes the backup artifact produced by ./make-backup.sh
# (work/backup.tar.gz), extracts it into ./restored/, installs the sqlite3
# .backup file as restored/app.db, and proves the restored database actually
# opens (PRAGMA integrity_check). Exit 0 = restored; any failure exits non-zero,
# which Salvage records as a normal FAIL verdict (not an operational error).
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ARTIFACT="$HERE/work/backup.tar.gz"
DEST="$HERE/restored"

if [ ! -f "$ARTIFACT" ]; then
  echo "backup artifact $ARTIFACT missing — run ./make-backup.sh first" >&2
  exit 1
fi

mkdir -p "$DEST"
tar -xzf "$ARTIFACT" -C "$DEST"
mv -f "$DEST/app.db.backup" "$DEST/app.db"

# A restore only counts if the restored database opens and is coherent.
if [ "$(sqlite3 "$DEST/app.db" 'PRAGMA integrity_check;')" != "ok" ]; then
  echo "restored database failed PRAGMA integrity_check" >&2
  exit 1
fi

echo "restored $ARTIFACT -> $DEST/app.db"
