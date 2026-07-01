#!/usr/bin/env bash
# Reproduce a local pgBackRest backup for exercising Salvage's physical restore.
#
# Builds a postgres+pgbackrest image, runs a primary with WAL archiving into a
# local repo volume, seeds demo data, and takes a full backup. The repo is left
# in the 'salvage-pgbr-repo' docker volume. Then:
#
#   salvage run -config salvage.pgbackrest.example.yaml
set -euo pipefail

IMAGE="${IMAGE:-salvage-pg-pgbackrest:16}"
STANZA="${STANZA:-demo}"
REPO_VOL="${REPO_VOL:-salvage-pgbr-repo}"
PRIMARY="salvage-pgbr-primary"
HERE="$(cd "$(dirname "$0")" && pwd)"

echo ">> build $IMAGE"
docker build -q -t "$IMAGE" "$HERE" >/dev/null

echo ">> fresh repo volume + primary"
docker rm -f "$PRIMARY" >/dev/null 2>&1 || true
docker volume rm "$REPO_VOL" >/dev/null 2>&1 || true
docker volume create "$REPO_VOL" >/dev/null
docker run -d --rm --name "$PRIMARY" \
  -e POSTGRES_PASSWORD=seed -e POSTGRES_DB=seeddb \
  -v "$REPO_VOL":/var/lib/pgbackrest \
  "$IMAGE" \
  postgres -c archive_mode=on -c "archive_command=pgbackrest --stanza=$STANZA archive-push %p" \
           -c max_wal_senders=3 -c wal_level=replica >/dev/null

echo ">> wait for readiness"
for i in $(seq 1 120); do
  if [ "$(docker exec -e PGPASSWORD=seed "$PRIMARY" psql -h 127.0.0.1 -U postgres -d seeddb -tAc 'select 1' 2>/dev/null | tr -d '[:space:]')" = "1" ]; then
    break
  fi
  [ "$i" = 120 ] && { echo "primary never became ready"; docker logs "$PRIMARY" 2>&1 | tail -20; exit 1; }
done

echo ">> stanza-create + check"
docker exec -u postgres "$PRIMARY" pgbackrest --stanza="$STANZA" stanza-create
docker exec -u postgres "$PRIMARY" pgbackrest --stanza="$STANZA" check

echo ">> seed demo data"
docker exec -e PGPASSWORD=seed "$PRIMARY" psql -h 127.0.0.1 -U postgres -d seeddb -v ON_ERROR_STOP=1 -c "
CREATE TABLE orders (id serial primary key, amount numeric(8,2), created_at timestamptz default now());
INSERT INTO orders (amount, created_at) SELECT (random()*100)::numeric(8,2), now() - (g||' hours')::interval FROM generate_series(0,9) g;
CREATE TABLE schema_migrations (version text primary key);
INSERT INTO schema_migrations VALUES ('20260615000000');
"

echo ">> full backup"
docker exec -u postgres "$PRIMARY" pgbackrest --stanza="$STANZA" --type=full backup

echo ">> repo info"
docker exec -u postgres "$PRIMARY" pgbackrest --stanza="$STANZA" info

docker kill "$PRIMARY" >/dev/null
echo ">> done — repo in volume '$REPO_VOL'. Run: salvage run -config salvage.pgbackrest.example.yaml"
