#!/usr/bin/env bash
# Reproduce a local MySQL logical dump for exercising Salvage's mysql restore-test
# (spec 0024). Seeds a throwaway MySQL container with a known table (a known row
# count), then `mysqldump`s it to dev/mysql/seed-dump.sql. Everything runs in the
# mysql:8 image, so no host mysql client is needed. Then:
#
#   salvage run -config salvage.mysql.example.yaml
set -euo pipefail

IMAGE="${IMAGE:-mysql:8.4.10}"
CONTAINER="salvage-mysql-seed"
DB="seeddb"
PASS="seed"
HERE="$(cd "$(dirname "$0")" && pwd)"
OUT="$HERE/seed-dump.sql"

echo ">> fresh seed container"
docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
docker run -d --rm --name "$CONTAINER" \
  -e MYSQL_ROOT_PASSWORD="$PASS" -e MYSQL_DATABASE="$DB" \
  "$IMAGE" >/dev/null

echo ">> wait for readiness"
for i in $(seq 1 120); do
  if [ "$(docker exec -e MYSQL_PWD="$PASS" "$CONTAINER" mysql -h 127.0.0.1 -u root -N -B -e 'select 1' 2>/dev/null | tr -d '[:space:]')" = "1" ]; then
    break
  fi
  [ "$i" = 120 ] && { echo "mysql never became ready"; docker logs "$CONTAINER" 2>&1 | tail -20; exit 1; }
  sleep 1
done

echo ">> seed demo data"
# -i is required: without it `docker exec` does not attach stdin, the heredoc is
# silently discarded, and mysql exits 0 having executed nothing (empty dump).
docker exec -i -e MYSQL_PWD="$PASS" "$CONTAINER" mysql -h 127.0.0.1 -u root "$DB" <<'SQL'
CREATE TABLE orders (
  id INT AUTO_INCREMENT PRIMARY KEY,
  amount DECIMAL(8,2),
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
INSERT INTO orders (amount, created_at) VALUES
  (10.00, NOW()), (20.00, NOW() - INTERVAL 1 HOUR), (30.00, NOW() - INTERVAL 2 HOUR),
  (40.00, NOW() - INTERVAL 3 HOUR), (50.00, NOW() - INTERVAL 4 HOUR),
  (60.00, NOW() - INTERVAL 5 HOUR), (70.00, NOW() - INTERVAL 6 HOUR),
  (80.00, NOW() - INTERVAL 7 HOUR), (90.00, NOW() - INTERVAL 8 HOUR),
  (100.00, NOW() - INTERVAL 9 HOUR);
CREATE TABLE schema_migrations (version VARCHAR(32) PRIMARY KEY);
INSERT INTO schema_migrations VALUES ('20260615000000');
SQL

echo ">> mysqldump -> $OUT"
docker exec -e MYSQL_PWD="$PASS" "$CONTAINER" mysqldump -h 127.0.0.1 -u root \
  --databases "$DB" --add-drop-database > "$OUT"

docker kill "$CONTAINER" >/dev/null

cat <<EOF

>> done — dump written to $OUT (database '$DB', 10 orders rows).
   Run:
     salvage run -config salvage.mysql.example.yaml
EOF
