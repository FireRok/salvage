# CI integration

The most common way to run Salvage unattended is as a **scheduled CI job**:
restore-test a backup on a cadence, gate on the exit code, and keep the report
as a build artifact. Salvage's exit-code contract and machine output (spec
[0026](../../specs/0026-machine-readable-output.md)) exist precisely for this.
This chapter gives the generic shape and one complete worked example.

For cron/systemd scheduling on a host you control (and the notary's
dead-man's-switch, which catches the runner that silently stops running), see
[Scheduling & monitoring](./06-scheduling-and-monitoring.md) — the two
approaches compose: CI is just another scheduler.

## The generic shape

Any CI system reduces to the same three steps:

1. **A scheduled trigger** — nightly or weekly, on the cadence you want to be
   able to claim.
2. **`salvage run -config …`** — the job step. The **exit code is the gate**:

   | Code | Meaning |
   |-----:|---------|
   | `0` | **pass** — restore succeeded and every required check passed |
   | `1` | **fail** — restore failed or a required check failed (a *result*, not a crash) |
   | `2` | **error** — operational problem (bad config, Docker unavailable, missing secret) |

   Every CI system already fails a job on a non-zero exit — no wrapper needed.
   If you want to distinguish "backup is bad" (1) from "the job itself is
   misconfigured" (2), branch on the exact code.
3. **Capture the report.** Either point `report.out` at a workspace path and
   upload it as the build artifact, or run with `-json` and redirect stdout —
   the bytes are identical, a single JSON document with `schema_version`
   (currently `1`).

Two operational notes CI users hit first:

- **The runner needs a reachable Docker daemon** for every engine except
  `exec` (the container engines — postgres, mysql, mongodb, restic, borg —
  restore into a disposable container). GitHub-hosted Linux runners ship one;
  on self-hosted runners, `salvage check` preflights it. `target.type: exec`
  runs the restore on the runner itself and needs no Docker.
- **Secrets flow by name, never by value.** List the variable names under
  `source.pass_env` in the config, and map your CI secret store into
  environment variables of those names in the job. The values never appear in
  the config, the repo, or any command line (spec
  [0003](../../specs/0003-security-and-isolation.md)).

## Worked example — GitHub Actions

A weekly restore-test of a restic repo, gating on the exit code and keeping
the report as an artifact:

```yaml
# .github/workflows/restore-test.yml
name: restore-test
on:
  schedule:
    - cron: "17 5 * * 1"   # weekly, Monday 05:17 UTC
  workflow_dispatch: {}     # allow manual runs too

jobs:
  restore-test:
    runs-on: ubuntu-latest  # GitHub-hosted runners have a Docker daemon
    steps:
      - uses: actions/checkout@v4   # brings salvage.yaml (and the binary, if vendored)

      - name: Install salvage
        run: |
          # Pin the release your pipeline is tested against. Release archives
          # are named salvage_<version>_<os>_<arch>.tar.gz on the Releases
          # page; alternatively, build from source with Go 1.23+ (`make build`).
          curl -fsSLo salvage.tar.gz "<release-url>/salvage_<version>_linux_amd64.tar.gz"
          tar xzf salvage.tar.gz salvage
          sudo install -m 0755 salvage /usr/local/bin/salvage

      - name: Restore-test the latest backup
        env:
          # Names must match source.pass_env in salvage.yaml — values stay in
          # the CI secret store, never in the config.
          RESTIC_REPOSITORY: ${{ secrets.RESTIC_REPOSITORY }}
          RESTIC_PASSWORD:   ${{ secrets.RESTIC_PASSWORD }}
          AWS_ACCESS_KEY_ID:     ${{ secrets.RESTIC_S3_KEY }}
          AWS_SECRET_ACCESS_KEY: ${{ secrets.RESTIC_S3_SECRET }}
        run: |
          salvage check -config salvage.yaml
          salvage run -json -config salvage.yaml > salvage-report.json

      - name: Keep the report
        if: always()        # keep the evidence on failure too
        uses: actions/upload-artifact@v4
        with:
          name: salvage-report
          path: salvage-report.json
```

The job fails when the verdict fails — `salvage run` exits `1` — and the
uploaded report says exactly which check failed and why.

**Translating to another CI** (GitLab CI, Jenkins, Forgejo/Gitea Actions,
Woodpecker, …) is mechanical, because the pattern is three lines of any CI's
YAML: a schedule trigger, secrets mapped to the env names in `pass_env`, and a
step that runs `salvage run` and fails the job on non-zero exit. The only
environmental requirement to verify is the Docker daemon on the runner (again,
except for `exec` targets).

## Attesting from CI

To turn the CI run into an independently counter-signed record, run
`salvage attest` instead of (or after) `salvage run`: it runs the same
restore-test, then submits the signed report to the hosted notary, which
counter-signs it and appends it to your tamper-evident ledger. The notary
attests to the report **your job generated** — it does not run the restore.

```yaml
      - name: Restore-test and attest
        env:
          RESTIC_PASSWORD:    ${{ secrets.RESTIC_PASSWORD }}
          SALVAGE_ATTEST_KEY: ${{ secrets.SALVAGE_ATTEST_KEY }}   # API key, by name
        run: salvage attest -config salvage.yaml
```

The API key comes from your CI secret store via the environment (default
variable: `SALVAGE_ATTEST_KEY`; see [Attestation](./05-attestation.md)) — the
config file only ever names the variable. A scheduled `attest` job also feeds
the notary's dead-man's-switch: if the pipeline stops running, the missed
cadence is alerted on even though the dead runner sent nothing.

## Parsing the report

`salvage run -json` writes the full report to stdout — one JSON document,
`schema_version: 1` — so downstream steps can extract fields without parsing
human text:

```sh
salvage run -json -config salvage.yaml > report.json || rc=$?
jq -r '.verdict'                            report.json   # pass | fail
jq -r '.checks[] | select(.ok|not) | .name' report.json   # failing checks
```

`salvage verify -json <id>` does the same for attestation verification (a
`valid` boolean plus the verification transcript), so a pipeline can also
re-verify a published attestation offline. See
[Commands](./04-commands.md#run) for both flags.
