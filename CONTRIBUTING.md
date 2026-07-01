# Contributing to Salvage

Thanks for your interest. Salvage is **Fair Source**, not open source — it's
licensed under the [Functional Source License](./LICENSE) (FSL-1.1-ALv2). You can
read, run, self-host, and modify it freely for any purpose **except** offering it
as a competing commercial service. Each release converts to Apache 2.0 two years
after publication.

## Developer Certificate of Origin

To keep relicensing and the future commercial offering clean, all contributions
require a sign-off. Add `Signed-off-by: Your Name <you@example.com>` to each
commit (`git commit -s`). This certifies the [DCO](https://developercertificate.org/).

> A full CLA may replace the DCO before the first external contribution is merged.
> If so, this section will be updated.

## Development

```sh
make tidy     # fetch dependencies (one: gopkg.in/yaml.v3)
make build    # produce ./salvage
make test
make vet
```

Salvage shells out to `docker` for the ephemeral Postgres environment — no host
Postgres client is required. Have Docker running to exercise `salvage run`.

## Scope

Salvage proves a backup **restores and works** (Level 3), which the integrity
checks in restic/borg/kopia deliberately stop short of. Keep that focus: this is
a verification + attestation layer *on top of* whatever backup tooling you
already run — not another backup tool.
