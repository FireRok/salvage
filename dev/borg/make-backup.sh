#!/usr/bin/env bash
# Reproduce a local borg repo for exercising Salvage's filesystem restore-test
# (spec 0022). Creates a dir of known test files (a known count, a known
# checksum), inits a borg repo in a docker volume, and archives the files.
#
# Everything runs in the borgbackup image, so no host borg is needed. The repo is
# left in the 'salvage-borg-repo' docker volume. Then:
#
#   export BORG_PASSPHRASE=salvage-dev
#   salvage run -config salvage.borg.example.yaml
set -euo pipefail

# NOTE: there is no official borgbackup Docker image — ghcr.io/borgbackup/borg
# does not exist (GHCR refuses even anonymous pull tokens). The borgmatic
# collective image ships borg on PATH, so we pin the entrypoint to `borg`.
IMAGE="${IMAGE:-ghcr.io/borgmatic-collective/borgmatic:2.1.6}"
REPO_VOL="${REPO_VOL:-salvage-borg-repo}"
SEED_VOL="${SEED_VOL:-salvage-borg-seed}"
REPO_PATH="/repo"                       # where the repo volume mounts in-container
SEED_PATH="/seed"                       # where the seed volume mounts in-container
PASSPHRASE="${BORG_PASSPHRASE:-salvage-dev}"
ARCHIVE="${ARCHIVE:-seed}"

echo ">> fresh repo + seed volumes"
docker volume rm "$REPO_VOL" "$SEED_VOL" >/dev/null 2>&1 || true
docker volume create "$REPO_VOL" >/dev/null
docker volume create "$SEED_VOL" >/dev/null

# Stage known test files inside the seed VOLUME (not a host bind mount — those are
# flaky on Docker Desktop for macOS). A config file with deterministic content
# (→ deterministic sha256) and three CSV data files (→ a known file_count of 3).
echo ">> stage known test files in volume $SEED_VOL"
docker run --rm -v "$SEED_VOL":"$SEED_PATH" alpine sh -c '
  set -e
  mkdir -p '"$SEED_PATH"'/etc '"$SEED_PATH"'/data
  printf "name = salvage-demo\nversion = 1\n" > '"$SEED_PATH"'/etc/app.conf
  printf "id,amount\n1,10\n2,20\n"             > '"$SEED_PATH"'/data/orders.csv
  printf "id,name\n1,alice\n2,bob\n"           > '"$SEED_PATH"'/data/customers.csv
  printf "version\n20260701\n"                 > '"$SEED_PATH"'/data/seed.csv
  echo "data/seed.csv sha256: $(sha256sum '"$SEED_PATH"'/data/seed.csv | cut -d" " -f1)"
'

# Run borg with the seed + repo volumes mounted; init then create. The
# entrypoint is pinned to `borg` (works for any image with borg on PATH), so we
# pass borg subcommands directly. BORG_REPO points at the mounted repo; the
# archive is addressed as ::<name>.
run_borg() {
  docker run --rm --entrypoint borg \
    -e "BORG_PASSPHRASE=$PASSPHRASE" \
    -e "BORG_REPO=$REPO_PATH" \
    -v "$REPO_VOL":"$REPO_PATH" \
    -v "$SEED_VOL":"$SEED_PATH":ro \
    "$IMAGE" "$@"
}

echo ">> borg init (repokey-blake2)"
run_borg init --encryption=repokey-blake2

echo ">> borg create ::$ARCHIVE $SEED_PATH"
run_borg create "::$ARCHIVE" "$SEED_PATH"

echo ">> borg list"
run_borg list

cat <<EOF

>> done — repo in volume '$REPO_VOL' (passphrase '$PASSPHRASE', archive '$ARCHIVE').
   The archived tree extracts under /restore as: seed/etc/app.conf, seed/data/*.csv
   Run:
     export BORG_PASSPHRASE=$PASSPHRASE
     salvage run -config salvage.borg.example.yaml
EOF
