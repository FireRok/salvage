#!/usr/bin/env bash
# Fetch the Pagila sample database (Postgres DVD-rental schema + data).
# Source: https://github.com/devrimgunduz/pagila  (license: postgresql-style, see repo)
# Downloads schema + data SQL into dev/corpus/.cache/pagila/. Idempotent.
set -euo pipefail

name="pagila"
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cache="$(cd "$here/../.." && pwd)/.cache/$name"
mkdir -p "$cache"

base="https://raw.githubusercontent.com/devrimgunduz/pagila/master"
schema_url="$base/pagila-schema.sql"
data_url="$base/pagila-data.sql"

fetch() {
  local url="$1" out="$2"
  if [[ -s "$out" ]]; then
    echo "[$name] already have $(basename "$out")"
    return 0
  fi
  echo "[$name] downloading $url"
  curl -fsSL "$url" -o "$out.tmp"
  mv "$out.tmp" "$out"
}

fetch "$schema_url" "$cache/pagila-schema.sql"
fetch "$data_url" "$cache/pagila-data.sql"

echo "[$name] ready in $cache"
ls -l "$cache"
