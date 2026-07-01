#!/usr/bin/env bash
# Salvage corpus runner.
#
# For each corpus database: fetch -> load into a source Postgres -> pg_dump -Fc ->
# run `salvage run` (does it restore?) and `salvage scaffold` (does introspection +
# check generation work?). Prints a results table. See manifest.yaml for entries.
#
# Usage:  ./run.sh [name ...]      (default: all entries)
#   SALVAGE=/path/to/salvage ./run.sh kitchensink pagila
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
SALVAGE="${SALVAGE:-$HOME/go/bin/salvage}"
TMP="$(mktemp -d)"
declare -a RESULTS
pass=x         # postgres superuser password for the throwaway source containers
CID=""         # current source container (set by run_entry; read by load_* funcs)

wait_ready() { # container db
  local i out
  for i in $(seq 1 150); do
    out=$(docker exec -e PGPASSWORD="$pass" "$1" psql -h 127.0.0.1 -U postgres -d "$2" -tAc 'select 1' 2>/dev/null | tr -d '[:space:]')
    [ "$out" = "1" ] && return 0
  done
  return 1
}

psqlc() { docker exec -e PGPASSWORD="$pass" "$CID" psql -h 127.0.0.1 -U postgres -d "$1" -v ON_ERROR_STOP=1 "${@:2}"; }

# Per-entry loaders. They read the global $CID and load into the named database.
load_sqlfiles() { # db file...
  local db="$1"; shift
  local f
  for f in "$@"; do
    docker cp "$HERE/$f" "$CID:/tmp/$(basename "$f")" >/dev/null || return 1
    psqlc "$db" -f "/tmp/$(basename "$f")" >/dev/null 2>"$TMP/loaderr" || { tail -3 "$TMP/loaderr" | sed 's/^/      /'; return 1; }
  done
}
load_timescale() {
  psqlc corpus -c 'CREATE EXTENSION IF NOT EXISTS timescaledb;' >/dev/null 2>&1 || return 1
  load_sqlfiles corpus .cache/timescale/weather.sql || return 1
  docker cp "$HERE/.cache/timescale/weather_small_locations.csv"  "$CID:/tmp/loc.csv"  >/dev/null
  docker cp "$HERE/.cache/timescale/weather_small_conditions.csv" "$CID:/tmp/cond.csv" >/dev/null
  psqlc corpus -c "\copy locations FROM '/tmp/loc.csv' CSV"   >/dev/null 2>"$TMP/loaderr" || { tail -3 "$TMP/loaderr"|sed 's/^/      /'; return 1; }
  psqlc corpus -c "\copy conditions FROM '/tmp/cond.csv' CSV" >/dev/null 2>"$TMP/loaderr" || { tail -3 "$TMP/loaderr"|sed 's/^/      /'; return 1; }
}
load_postgis() {
  psqlc corpus -c 'CREATE EXTENSION IF NOT EXISTS postgis;' >/dev/null 2>&1 || return 1
  local b
  for b in shp dbf prj shx cpg; do
    docker cp "$HERE/.cache/postgis/ne_110m_admin_0_countries.$b" "$CID:/tmp/ne.$b" >/dev/null 2>/dev/null || true
  done
  docker exec -e PGPASSWORD="$pass" "$CID" bash -c \
    "shp2pgsql -s 4326 -I /tmp/ne.shp countries | psql -h 127.0.0.1 -U postgres -d corpus -v ON_ERROR_STOP=1" \
    >/dev/null 2>"$TMP/loaderr" || { tail -3 "$TMP/loaderr"|sed 's/^/      /'; return 1; }
}

run_entry() { # name image src_db restore_image preload load_cmd...
  local name="$1" image="$2" sdb="$3" rimage="$4" preload="$5"; shift 5
  printf '\n### %s  (%s)\n' "$name" "$image"

  docker pull -q "$image" >/dev/null 2>&1 || true
  CID=$(docker run -d --rm -e POSTGRES_PASSWORD="$pass" -e POSTGRES_DB=corpus --shm-size=256m "$image" 2>/dev/null)
  if [ -z "$CID" ] || ! wait_ready "$CID" postgres; then
    echo "  source container never ready"; [ -n "$CID" ] && docker kill "$CID" >/dev/null 2>&1
    RESULTS+=("$name|setup-fail|-"); return
  fi

  if ! "$@"; then
    echo "  LOAD failed"; docker kill "$CID" >/dev/null 2>&1; RESULTS+=("$name|load-fail|-"); return
  fi

  local dump="$TMP/$name.dump"
  if ! docker exec -e PGPASSWORD="$pass" "$CID" pg_dump -h 127.0.0.1 -U postgres -Fc -d "$sdb" -f /tmp/d.dump >/dev/null 2>"$TMP/dumperr"; then
    echo "  pg_dump failed:"; sed 's/^/    /' "$TMP/dumperr"; docker kill "$CID" >/dev/null 2>&1; RESULTS+=("$name|dump-fail|-"); return
  fi
  docker cp "$CID:/tmp/d.dump" "$dump" >/dev/null
  docker kill "$CID" >/dev/null 2>&1
  echo "  pg_dump -Fc: $(du -h "$dump" | cut -f1)"

  local cfg="$TMP/$name.yaml"
  {
    echo "target:"
    echo "  name: $name"
    echo "  type: postgres"
    echo "  source: {kind: pg_dump, path: $dump}"
    echo "  restore:"
    echo "    image: $rimage"
    [ -n "$preload" ] && echo "    preload_libraries: [$preload]"
    echo "    timeout: 15m"
    echo "report: {out: $TMP/$name-report.json}"
  } > "$cfg"

  local rrun rscaf
  if "$SALVAGE" run -config "$cfg" >"$TMP/$name-run.log" 2>&1; then rrun="PASS"; else rrun="FAIL"; fi
  if "$SALVAGE" scaffold -config "$cfg" -o "$TMP/$name-scaffold.yaml" >"$TMP/$name-scaf.log" 2>&1; then rscaf="ok"; else rscaf="fail"; fi
  printf '  salvage restore=%s  scaffold=%s\n' "$rrun" "$rscaf"
  if [ "$rrun" = FAIL ]; then echo "    --- run tail ---"; tail -4 "$TMP/$name-run.log" | sed 's/^/    /'; fi
  RESULTS+=("$name|$rrun|$rscaf")
}

# Fetch everything first (idempotent).
for f in "$HERE"/sources/*/fetch.sh; do bash "$f" >/dev/null 2>&1 || echo "fetch warn: $f"; done

want=("$@"); [ ${#want[@]} -eq 0 ] && want=(kitchensink pagila chinook northwind timescale postgis)
has() { local x; for x in "${want[@]}"; do [ "$x" = "$1" ] && return 0; done; return 1; }

has kitchensink && run_entry kitchensink postgres:16                       corpus         postgres:16                       ""          load_sqlfiles corpus kitchensink/kitchensink.sql
has pagila      && run_entry pagila      postgres:16                       corpus         postgres:16                       ""          load_sqlfiles corpus .cache/pagila/pagila-schema.sql .cache/pagila/pagila-data.sql
has chinook     && run_entry chinook     postgres:16                       chinook_serial postgres:16                      ""          load_sqlfiles postgres .cache/chinook/chinook.sql
has northwind   && run_entry northwind   postgres:16                       corpus         postgres:16                       ""          load_sqlfiles corpus .cache/northwind/northwind.sql
has timescale   && run_entry timescale   timescale/timescaledb:latest-pg16 corpus        timescale/timescaledb:latest-pg16 timescaledb load_timescale
has postgis     && run_entry postgis     postgis/postgis:16-3.4            corpus         postgis/postgis:16-3.4            ""          load_postgis

echo ""
echo "============ CORPUS RESULTS ============"
printf '%-14s %-12s %-9s\n' ENTRY RESTORE SCAFFOLD
failed=0
for r in "${RESULTS[@]}"; do
  IFS='|' read -r n a b <<<"$r"
  printf '%-14s %-12s %-9s\n' "$n" "$a" "$b"
  # Any entry that did not restore (PASS) and scaffold (ok) is a failure.
  [ "$a" = PASS ] && [ "$b" = ok ] || failed=$((failed + 1))
done
echo ""
echo "logs + generated configs under: $TMP"
if [ "$failed" -gt 0 ]; then
  echo "CORPUS: $failed of ${#RESULTS[@]} entries FAILED"
  exit 1
fi
echo "CORPUS: all ${#RESULTS[@]} entries passed"
