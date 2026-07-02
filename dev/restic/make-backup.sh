#!/usr/bin/env bash
# Reproduce a local restic repo for exercising Salvage's filesystem restore-test
# (spec 0018). Creates a dir of known test files (a known count, a known
# checksum), inits a restic repo in a docker volume, and backs the files up.
#
# Everything runs in the restic/restic image, so no host restic is needed. The
# repo is left in the 'salvage-restic-repo' docker volume. Then:
#
#   export RESTIC_PASSWORD=salvage-dev
#   salvage run -config salvage.restic.example.yaml
set -euo pipefail

IMAGE="${IMAGE:-restic/restic:0.19.0}"
REPO_VOL="${REPO_VOL:-salvage-restic-repo}"
SEED_VOL="${SEED_VOL:-salvage-restic-seed}"
REPO_PATH="/repo"                       # where the repo volume mounts in-container
SEED_PATH="/seed"                       # where the seed volume mounts in-container
PASSWORD="${RESTIC_PASSWORD:-salvage-dev}"

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

# Run restic with the seed + repo volumes mounted; init then backup. The
# restic/restic entrypoint IS restic, so we pass restic subcommands directly.
run_restic() {
  docker run --rm \
    -e "RESTIC_PASSWORD=$PASSWORD" \
    -e "RESTIC_REPOSITORY=$REPO_PATH" \
    -v "$REPO_VOL":"$REPO_PATH" \
    -v "$SEED_VOL":"$SEED_PATH":ro \
    "$IMAGE" "$@"
}

echo ">> restic init"
run_restic init

echo ">> restic backup $SEED_PATH"
run_restic backup "$SEED_PATH"

echo ">> restic snapshots"
run_restic snapshots

cat <<EOF

>> done — repo in volume '$REPO_VOL' (password '$PASSWORD').
   The backed-up tree restores under /restore as: seed/etc/app.conf, seed/data/*.csv
   Run:
     export RESTIC_PASSWORD=$PASSWORD
     salvage run -config salvage.restic.example.yaml
EOF
