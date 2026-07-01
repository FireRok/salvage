#!/usr/bin/env bash
# Fetch the Northwind sample database (classic orders/customers) ported to Postgres.
# Source: https://github.com/pthom/northwind_psql  (license: see repo; community port of MS sample)
# Single self-contained SQL file (schema + data). Idempotent.
set -euo pipefail

name="northwind"
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cache="$(cd "$here/../.." && pwd)/.cache/$name"
mkdir -p "$cache"

url="https://raw.githubusercontent.com/pthom/northwind_psql/master/northwind.sql"
out="$cache/northwind.sql"

if [[ -s "$out" ]]; then
  echo "[$name] already have $(basename "$out")"
else
  echo "[$name] downloading $url"
  curl -fsSL "$url" -o "$out.tmp"
  mv "$out.tmp" "$out"
fi

echo "[$name] ready in $cache"
ls -l "$cache"
