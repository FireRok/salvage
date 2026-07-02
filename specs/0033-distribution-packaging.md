# 0033 — Distribution & packaging

- **Status:** Implemented — full R4 tap automation awaits the HOMEBREW_TAP_TOKEN secret (release skips the tap push until set); first signed release = next tag (v0.2.0 predates signing)
- **Created:** 2026-07-01
- **Owner:** Firerok

## Context

Salvage already has the *skeleton* of a release pipeline but not a distribution
story an operator can actually follow. What exists today:

- **A GoReleaser config** (`.goreleaser.yaml`): CGO-free builds for
  `linux`/`darwin` × `amd64`/`arm64`, `tar.gz` archives named
  `salvage_{version}_{os}_{arch}.tar.gz`, a `checksums.txt`, and version
  injection via `-X salvage.sh/internal/version.{Version,Commit,Date}` — the
  same ldflags the `Makefile` uses locally (`Makefile:7-10`).
- **A tag-triggered release workflow** (`.github/workflows/release.yml`): pushes
  of `v*` tags to the public GitHub mirror run `goreleaser release --clean`.
- **Version embedding that survives every build path**
  (`internal/version/version.go`): ldflags for release builds, with a
  `debug.ReadBuildInfo` fallback so `go install salvage.sh/cmd/salvage@vX`
  still reports the tag. `salvage version` prints it
  (`cmd/salvage/main.go:54-55`).

What does *not* exist is everything between "an artifact appears on a GitHub
Release" and "an operator is running a trusted binary":

- **No install path other than building from source.** The README Quickstart
  (`README.md:190-198`) starts with `brew install go` and `make build`; the
  guide says only that "prebuilt binaries are published on GitHub releases"
  with no command to fetch one (`docs/guide/01-getting-started.md:30-32`).
  There is no install script and no Homebrew formula for Salvage itself.
- **No artifact signing.** `checksums.txt` is unsigned, so it authenticates
  nothing: whoever can substitute the archive can substitute the checksum file
  beside it. For most CLIs this is a nice-to-have; for Salvage it is close to
  load-bearing. The product's core claim is *verifiable evidence*, and
  `salvage verify` checks attestations offline against Firerok's public key
  (`cmd/salvage/main.go:78`). A tampered binary could misreport a
  verification result — the binary is part of the trust chain and should be
  verifiable with the same rigor Salvage applies to backups.
- **No update discovery.** `salvage version` reports what you have; nothing
  tells an operator (or an unattended `schedule`-driven host, [[spec 0015]])
  that a newer release exists.

This spec defines the public distribution contract: what a release ships, how
it is signed, and the supported install paths. It deliberately specs the
*artifacts and their guarantees*, not the private release tooling that produces
them — development is Forgejo-first with releases mirrored to public GitHub
(`README.md:237-244`), and that workflow is out of scope here. Release
*numbering, changelog, and cadence* are [[spec 0034]]; behavioral compatibility
across versions is [[spec 0035]].

## Goals

- Every tagged release publishes a **complete, predictable artifact set**:
  per-platform archives, a checksum manifest, and a signature over that
  manifest — so a third party can verify a download end-to-end without
  trusting the transport or the hosting.
- A **one-command install** (`curl … | sh` against a stable URL) that verifies
  checksums before installing, works on macOS and Linux, and never asks for
  more privilege than the install directory requires.
- A **Homebrew tap** so macOS (and Linuxbrew) users get `brew install` /
  `brew upgrade` semantics, updated automatically by the release pipeline.
- The **version-embedding invariant**: on every supported install path
  (install script, Homebrew, `go install`, source build), `salvage version`
  reports a truthful version; on release paths, exactly the released tag.
- An **opt-in update check** (`salvage version -check`) so unattended hosts
  can notice drift without any auto-update machinery.
- Documentation that puts prebuilt installs *first* and source builds last —
  the reverse of today's README.

## Non-goals

- **Auto-update.** Salvage never replaces its own binary. `-check` reports;
  the operator (or their package manager) acts. An unattended verification
  runner that silently swaps its own executable would undermine the very
  trust posture the signing work establishes.
- **Windows binaries.** The restore engines shell out to Docker and POSIX
  tooling; Windows support is unvalidated and deferred (Open question).
- **OS distribution packages** (deb/rpm/apk) and **third-party package
  repositories** (homebrew-core, nixpkgs, AUR). The own-tap path ships first;
  community/distro packaging can follow demand.
- **A container image of the Salvage CLI itself.** Attractive for CI users but
  it complicates the Docker-in-Docker story for the container engines;
  deferred (Open question).
- **Release numbering / changelog / cadence** — [[spec 0034]].

## Design

### Artifact set (extends `.goreleaser.yaml`)

Each release `vX.Y.Z` publishes to its GitHub Release:

```
salvage_X.Y.Z_linux_amd64.tar.gz
salvage_X.Y.Z_linux_arm64.tar.gz
salvage_X.Y.Z_darwin_amd64.tar.gz
salvage_X.Y.Z_darwin_arm64.tar.gz
checksums.txt          # SHA-256 of every archive (exists today)
checksums.txt.sig      # NEW — signature over checksums.txt
```

The naming template is already what `.goreleaser.yaml` produces; this spec
freezes it as a contract (scripts and the install script depend on it).

### Signing the checksum manifest

One signature over `checksums.txt` transitively covers every archive — the
standard, cheap pattern. Two workable schemes:

- **cosign keyless** (recommended): the release job signs with an ephemeral
  key bound to the GitHub Actions OIDC identity, verifiable against the public
  transparency log. No long-lived private key to custody; verification asserts
  *"signed by this repository's release workflow."* GoReleaser supports this
  natively (`signs:` with `cosign sign-blob`).
- **minisign/signify**: a small long-lived keypair, trivially verifiable
  offline with a ~100 KB tool, but now Firerok custodies a release signing key
  and must publish/rotate it.

The recommendation is cosign keyless (the release already runs in GitHub
Actions, so the identity binding is natural), with the final call an Open
question. Either way the *public verification procedure* is documented in the
install docs, and the install script performs checksum verification always,
signature verification when the verifying tool is present.

### Install script

A POSIX-sh script served at a stable, memorable URL (e.g.
`https://salvage.sh/install.sh`; exact host is an Open question — the module
path `salvage.sh` already implies the domain). Behavior:

1. Detect `uname -s` / `uname -m`, map to a released `{os}_{arch}` pair; fail
   with a clear message on unsupported platforms.
2. Resolve the latest release (or `SALVAGE_VERSION` if set) and download the
   matching archive **and** `checksums.txt` (+ `.sig`).
3. Verify the archive's SHA-256 against the manifest **before** unpacking;
   verify the manifest signature when `cosign` (or the chosen tool) is
   available, and say which level of verification happened.
4. Install to `SALVAGE_INSTALL_DIR` (default: a user-writable location such as
   `~/.local/bin`, falling back to `/usr/local/bin` only when writable). The
   script itself never invokes `sudo`.
5. Print the installed path and `salvage version` output.

Any failure (unsupported platform, checksum mismatch, partial download) exits
non-zero having installed nothing.

### Homebrew tap

A `firerok/homebrew-tap` repository (name is an Open question) receives an
auto-generated formula from GoReleaser's `brews:` block on each release, so
`brew install firerok/tap/salvage` and `brew upgrade salvage` track releases
with zero manual steps. The formula installs the prebuilt binary (checksummed
by Homebrew itself), not a source build.

### `salvage version -check`

`salvage version` stays fully offline exactly as today. With `-check`, the
command additionally queries a single stable endpoint for the latest released
version (the GitHub Releases API is sufficient and needs no new
infrastructure; a first-party endpoint can replace it later), compares, and
prints both versions. Exit codes follow the house convention
(`cmd/salvage/main.go:83-87` semantics): `0` = up to date, `1` = a newer
release exists (a *result*), `2` = the check could not be performed (network,
API error). The request carries nothing but the HTTP request itself — no
identifiers, no telemetry. Stdlib HTTP + JSON only; the single-dependency
posture (`gopkg.in/yaml.v3`, `README.md:194`) is preserved.

## Requirements

**R1 — Frozen artifact contract.** Every release `vX.Y.Z` MUST publish, on its
public GitHub Release: one `tar.gz` per supported platform following the
existing `salvage_{version}_{os}_{arch}.tar.gz` template, a `checksums.txt`
containing SHA-256 digests of every archive, and a signature artifact over
`checksums.txt`. The supported-platform set is at minimum
`linux`/`darwin` × `amd64`/`arm64` (as `.goreleaser.yaml` builds today).

**R2 — Verifiable signature.** The `checksums.txt` signature MUST be
verifiable by a third party using only public information (a published key or
a keyless identity + transparency log), and the verification procedure MUST be
documented in the public docs. A release whose signature does not verify MUST
be treated as invalid.

**R3 — Install script.** A POSIX-sh install script MUST be available at a
stable URL. It MUST: detect OS/arch and fail clearly when unsupported; verify
the downloaded archive against `checksums.txt` before unpacking (a mismatch
MUST abort with nothing installed); honor `SALVAGE_VERSION` and
`SALVAGE_INSTALL_DIR` overrides; default to a user-writable install directory;
and never invoke `sudo` itself.

**R4 — Homebrew tap.** A Firerok-owned tap MUST provide a `salvage` formula
that installs the release binary, and the release pipeline MUST update the
formula automatically for each release, with no manual step.

**R5 — Version-embedding invariant.** On every supported install path —
install script, Homebrew, `go install salvage.sh/cmd/salvage@vX.Y.Z`, and
`make build` from a release tag — `salvage version` MUST report `X.Y.Z`
(source builds off-tag report the `git describe` form as today,
`Makefile:3`). No install path may yield a binary that reports `0.0.0-dev`
for a released version.

**R6 — Opt-in update check.** `salvage version -check` MUST report the running
and latest released versions, exiting `0` when current, `1` when a newer
release exists, `2` when the check cannot be performed. Without `-check`,
`salvage version` MUST perform no network I/O. The check MUST NOT transmit any
data beyond the version-lookup request itself, and Salvage MUST NOT modify its
own binary under any circumstances.

**R7 — Docs lead with prebuilt installs.** `README.md` and
`docs/guide/01-getting-started.md` MUST present install paths in the order:
install script, Homebrew, `go install`, build-from-source — with copy-paste
commands for each. (Overlaps [[spec 0037]]'s parity rules; this requirement
owns the install section specifically.)

**R8 — No new runtime dependency.** The update check and version plumbing MUST
use only the Go standard library; the module's single third-party dependency
posture is unchanged.

## Open questions

- **Signing scheme.** cosign keyless (no key custody, identity-bound, heavier
  verifier tooling) vs minisign (tiny offline verifier, but a long-lived key
  to protect and publish). Recommended: cosign keyless; decide before the
  first signed release, since switching schemes later resets verifier habits.
- **Install-script hosting.** `salvage.sh/install.sh` (first-party, needs the
  site to serve it immutably-ish) vs the raw GitHub URL (works today, uglier
  and couples to repo layout). The stable URL can redirect initially.
- **Windows.** `GOOS=windows` cross-compiles, but the engines shell out to
  Docker/`tar`/POSIX shell in places; untested. Decide whether to ship an
  *unsupported* windows archive or none at all — a broken official binary is
  worse than absence.
- **Container image for CI.** A `ghcr.io/…/salvage` image would suit CI
  runners, but the container engines need a reachable Docker daemon
  (socket-mount or DinD), which deserves its own recipe — likely lands with
  [[spec 0037]]'s CI documentation rather than here.
- **Tap naming.** `firerok/homebrew-tap` (org-wide, room for future tools) vs
  `firerok/homebrew-salvage`.

## Acceptance criteria

1. Cutting a release tag produces a GitHub Release containing exactly the R1
   artifact set; downloading `checksums.txt` and its signature and running the
   documented verification succeeds (R1, R2).
2. On a clean macOS (arm64) and Linux (amd64) host, the documented one-line
   install command installs Salvage without `sudo`, and `salvage version`
   prints the release tag (R3, R5).
3. Corrupting one byte of a downloaded archive (or substituting a stale
   `checksums.txt`) causes the install script to abort non-zero with nothing
   installed (R3).
4. `brew install <tap>/salvage` installs the release binary; after the next
   release, `brew upgrade salvage` moves to it with no manual formula edit
   (R4).
5. `go install salvage.sh/cmd/salvage@vX.Y.Z && salvage version` reports
   `X.Y.Z` (R5).
6. `salvage version` with no flags performs no network I/O (verifiable with a
   network-denied sandbox); `salvage version -check` exits `0`/`1`/`2` per R6
   in the up-to-date / outdated / offline cases.
7. The README Quickstart no longer requires installing Go to obtain Salvage
   (R7).
