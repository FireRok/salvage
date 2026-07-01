#!/usr/bin/env bash
# Fetch the TimescaleDB "weather" sample dataset (hypertables + time-series).
# Source: TimescaleDB sample datasets, https://docs.timescale.com (Tiger Data).
# Archive contains weather_small.sql (creates a hypertable via create_hypertable)
# plus CSV data files copied into it. Must be loaded into a timescaledb image.
# Idempotent: downloads + extracts into dev/corpus/.cache/timescale/.
set -euo pipefail

name="timescale"
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cache="$(cd "$here/../.." && pwd)/.cache/$name"
mkdir -p "$cache"

url="https://assets.timescale.com/docs/downloads/weather_small.tar.gz"
tarball="$cache/weather_small.tar.gz"

if [[ -s "$tarball" ]]; then
  echo "[$name] already have $(basename "$tarball")"
else
  echo "[$name] downloading $url"
  curl -fsSL "$url" -o "$tarball.tmp"
  mv "$tarball.tmp" "$tarball"
fi

# Extract (the archive expands to weather.sql + weather_small_*.csv).
if [[ ! -s "$cache/weather.sql" ]]; then
  echo "[$name] extracting"
  tar -xzf "$tarball" -C "$cache"
fi

echo "[$name] ready in $cache"
ls -l "$cache"
