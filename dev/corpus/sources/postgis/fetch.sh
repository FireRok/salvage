#!/usr/bin/env bash
# Fetch a small PostGIS spatial dataset: Natural Earth 1:110m admin-0 countries.
# Source: Natural Earth (public domain), https://www.naturalearthdata.com
# Downloads + unzips the shapefile bundle into dev/corpus/.cache/postgis/.
# The runner loads it with shp2pgsql (bundled in the postgis/postgis image) into
# a PostGIS-enabled database. Idempotent.
set -euo pipefail

name="postgis"
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cache="$(cd "$here/../.." && pwd)/.cache/$name"
mkdir -p "$cache"

url="https://naciscdn.org/naturalearth/110m/cultural/ne_110m_admin_0_countries.zip"
zip="$cache/ne_110m_admin_0_countries.zip"

if [[ -s "$zip" ]]; then
  echo "[$name] already have $(basename "$zip")"
else
  echo "[$name] downloading $url"
  curl -fsSL "$url" -o "$zip.tmp"
  mv "$zip.tmp" "$zip"
fi

if [[ ! -s "$cache/ne_110m_admin_0_countries.shp" ]]; then
  echo "[$name] extracting"
  unzip -o -q "$zip" -d "$cache"
fi

echo "[$name] ready in $cache"
ls -l "$cache"
