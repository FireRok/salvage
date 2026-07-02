#!/usr/bin/env bash
# Reproduce a local MongoDB logical dump for exercising Salvage's mongodb
# restore-test (spec 0025). Seeds a throwaway MongoDB container with a known
# "orders" collection (a known document count), then `mongodump`s it to
# dev/mongodb/seed-dump.archive. Everything runs in the mongo:7 image, so no
# host mongosh/mongodump client is needed. Then:
#
#   salvage run -config salvage.mongodb.example.yaml
set -euo pipefail

IMAGE="${IMAGE:-mongo:7.0.37}"
CONTAINER="salvage-mongodb-seed"
DB="seeddb"
USER="root"
PASS="seed"
HERE="$(cd "$(dirname "$0")" && pwd)"
OUT="$HERE/seed-dump.archive"

echo ">> fresh seed container"
docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
docker run -d --rm --name "$CONTAINER" \
  -e MONGO_INITDB_ROOT_USERNAME="$USER" -e MONGO_INITDB_ROOT_PASSWORD="$PASS" \
  "$IMAGE" >/dev/null

echo ">> wait for readiness"
for i in $(seq 1 120); do
  if [ "$(docker exec "$CONTAINER" mongosh --quiet -u "$USER" -p "$PASS" --authenticationDatabase admin --eval 'db.runCommand({ping:1}).ok' 2>/dev/null | tr -d '[:space:]')" = "1" ]; then
    break
  fi
  [ "$i" = 120 ] && { echo "mongodb never became ready"; docker logs "$CONTAINER" 2>&1 | tail -20; exit 1; }
  sleep 1
done

echo ">> seed demo data"
docker exec "$CONTAINER" mongosh --quiet -u "$USER" -p "$PASS" --authenticationDatabase admin "$DB" --eval '
db.orders.insertMany([
  {status: "shipped", amount: 10, createdAt: new Date()},
  {status: "shipped", amount: 20, createdAt: new Date(Date.now() - 3600*1000)},
  {status: "pending", amount: 30, createdAt: new Date(Date.now() - 7200*1000)},
  {status: "shipped", amount: 40, createdAt: new Date(Date.now() - 10800*1000)},
  {status: "cancelled", amount: 50, createdAt: new Date(Date.now() - 14400*1000)},
]);
db.orders.insertOne({_id: "o1", status: "shipped", amount: 99, createdAt: new Date()});
db.meta.insertOne({_id: "schema", version: 3});
' >/dev/null

echo ">> mongodump --archive -> $OUT"
docker exec "$CONTAINER" mongodump --quiet -u "$USER" -p "$PASS" --authenticationDatabase admin \
  --db "$DB" --archive > "$OUT"

docker kill "$CONTAINER" >/dev/null

cat <<EOF

>> done — dump written to $OUT (database '$DB', 6 orders docs incl. o1).
   Run:
     salvage run -config salvage.mongodb.example.yaml
EOF
