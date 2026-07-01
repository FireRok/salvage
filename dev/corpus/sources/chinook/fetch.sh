#!/usr/bin/env bash
# Fetch the Chinook sample database (digital media store) for Postgres.
# Source: https://github.com/lerocha/chinook-database  (license: MIT)
# Single self-contained SQL file (schema + data). Idempotent.
set -euo pipefail

name="chinook"
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cache="$(cd "$here/../.." && pwd)/.cache/$name"
mkdir -p "$cache"

# SerialPKs variant uses native SERIAL/identity columns (cleaner for a fresh load).
url="https://raw.githubusercontent.com/lerocha/chinook-database/master/ChinookDatabase/DataSources/Chinook_PostgreSql_SerialPKs.sql"
out="$cache/chinook.sql"

if [[ -s "$out" ]]; then
  echo "[$name] already have $(basename "$out")"
else
  echo "[$name] downloading $url"
  curl -fsSL "$url" -o "$out.tmp"
  mv "$out.tmp" "$out"
fi

echo "[$name] ready in $cache"
ls -l "$cache"
