#!/bin/sh
# Salvage installer — https://salvage.sh   (spec 0033 R3)
#
# Downloads the latest (or pinned) Salvage release binary for this platform
# from GitHub releases, verifies its SHA-256 against the release's
# checksums.txt BEFORE unpacking, additionally verifies the checksums.txt
# cosign signature when cosign is installed, and installs the `salvage`
# binary. Any failure exits non-zero with nothing installed.
#
#   curl -fsSL https://salvage.sh/install.sh | sh
#
# Environment overrides:
#   SALVAGE_VERSION        release tag to install (e.g. v0.2.0); default: latest
#   SALVAGE_INSTALL_DIR    install directory; default: /usr/local/bin when
#                          writable (no sudo — ever), else ~/.local/bin
#   SALVAGE_DOWNLOAD_BASE  alternate base URL for the release assets
#                          (mirrors / testing); default: the GitHub release
#
# POSIX sh (dash-clean). This script never invokes sudo: if the install
# directory is not writable, re-run with SALVAGE_INSTALL_DIR pointing at one
# that is.
set -eu

REPO="firerok/salvage"
API_LATEST="https://api.github.com/repos/${REPO}/releases/latest"

say()  { printf '%s\n' "$*"; }
fail() { printf 'salvage install: ERROR: %s\n' "$*" >&2; exit 1; }

# --- downloader: curl or wget, whichever exists -------------------------------
if command -v curl >/dev/null 2>&1; then
  fetch() { curl -fsSL -o "$2" "$1"; }
  fetch_stdout() { curl -fsSL "$1"; }
elif command -v wget >/dev/null 2>&1; then
  fetch() { wget -q -O "$2" "$1"; }
  fetch_stdout() { wget -q -O - "$1"; }
else
  fail "need curl or wget to download the release"
fi

# --- platform detection --------------------------------------------------------
os=$(uname -s)
case "$os" in
  Linux)  os=linux ;;
  Darwin) os=darwin ;;
  *) fail "unsupported operating system: $os — Salvage ships linux and darwin binaries; see https://github.com/${REPO} to build from source" ;;
esac

arch=$(uname -m)
case "$arch" in
  x86_64|amd64)  arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) fail "unsupported architecture: $arch — Salvage ships amd64 and arm64 binaries; see https://github.com/${REPO} to build from source" ;;
esac

# --- resolve the version -------------------------------------------------------
ver="${SALVAGE_VERSION:-}"
if [ -z "$ver" ]; then
  ver=$(fetch_stdout "$API_LATEST" | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)
  [ -n "$ver" ] || fail "could not resolve the latest release from $API_LATEST (set SALVAGE_VERSION=vX.Y.Z to pin one)"
fi
case "$ver" in v*) ;; *) ver="v$ver" ;; esac
vnum=${ver#v}

base="${SALVAGE_DOWNLOAD_BASE:-https://github.com/${REPO}/releases/download/${ver}}"
tarball="salvage_${vnum}_${os}_${arch}.tar.gz"

tmp=$(mktemp -d) || fail "mktemp failed"
trap 'rm -rf "$tmp"' EXIT INT TERM

say "salvage install: ${ver} (${os}/${arch})"
say "  downloading ${tarball} ..."
fetch "${base}/${tarball}" "${tmp}/${tarball}" || fail "download failed: ${base}/${tarball}"
fetch "${base}/checksums.txt" "${tmp}/checksums.txt" || fail "download failed: ${base}/checksums.txt"

# --- checksum verification: always, BEFORE unpacking (spec 0033 R3) -----------
expected=$(awk -v f="$tarball" '$2 == f { print $1 }' "${tmp}/checksums.txt")
[ -n "$expected" ] || fail "checksums.txt has no entry for ${tarball} — refusing to install"

if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "${tmp}/${tarball}" | awk '{ print $1 }')
elif command -v shasum >/dev/null 2>&1; then
  actual=$(shasum -a 256 "${tmp}/${tarball}" | awk '{ print $1 }')
else
  fail "need sha256sum or shasum to verify the download — refusing to install unverified binaries"
fi

if [ "$expected" != "$actual" ]; then
  {
    printf 'salvage install: CHECKSUM MISMATCH for %s\n' "$tarball"
    printf '  expected (checksums.txt): %s\n' "$expected"
    printf '  actual   (downloaded):    %s\n' "$actual"
    printf '  The download does not match the release manifest. Aborting —\n'
    printf '  NOTHING was installed and the archive was NOT unpacked.\n'
    printf '  Retry; if the mismatch persists, treat the artifact as\n'
    printf '  untrusted and report it: https://github.com/%s/issues\n' "$REPO"
  } >&2
  exit 1
fi
say "  checksum ok (sha256 matches checksums.txt)"

# --- signature verification: when cosign is available (spec 0033 R2/R3) -------
# One keyless cosign signature over checksums.txt covers every archive. The
# sigstore bundle (checksums.txt.sigstore.json) carries the signature, the
# ephemeral certificate bound to the firerok/salvage release workflow's OIDC
# identity, and the transparency-log proof — verifiable with only public
# information (cosign >= 2.4). Absent cosign (or a release predating signing,
# e.g. v0.2.0), the install proceeds on checksum verification alone and says so.
verified="sha256 checksum"
if command -v cosign >/dev/null 2>&1; then
  if fetch "${base}/checksums.txt.sigstore.json" "${tmp}/checksums.txt.sigstore.json" 2>/dev/null; then
    if cosign verify-blob \
      --bundle "${tmp}/checksums.txt.sigstore.json" \
      --certificate-identity-regexp "^https://github\\.com/firerok/salvage/\\.github/workflows/release\\.yml@" \
      --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
      "${tmp}/checksums.txt" >/dev/null 2>&1; then
      verified="sha256 checksum + cosign signature"
      say "  signature ok (cosign keyless: firerok/salvage release workflow)"
    else
      fail "cosign signature verification FAILED for checksums.txt (needs cosign >= 2.4; if your cosign is current, treat this release as invalid) — nothing was installed"
    fi
  else
    say "  note: ${ver} publishes no signature assets — verified by checksum only"
  fi
else
  say "  note: cosign not installed — skipping signature verification (checksum still verified)"
fi

# --- unpack + install ----------------------------------------------------------
(cd "$tmp" && tar -xzf "$tarball") || fail "could not unpack ${tarball}"
[ -f "${tmp}/salvage" ] || fail "archive did not contain the salvage binary"

dir="${SALVAGE_INSTALL_DIR:-}"
if [ -z "$dir" ]; then
  if [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then
    dir=/usr/local/bin
  else
    dir="${HOME}/.local/bin"
  fi
fi
mkdir -p "$dir" 2>/dev/null || fail "cannot create ${dir} (set SALVAGE_INSTALL_DIR to a writable directory; this script never uses sudo)"
[ -w "$dir" ] || fail "${dir} is not writable (set SALVAGE_INSTALL_DIR to a writable directory; this script never uses sudo)"

# Stage inside the target directory, then rename — no partial binary on PATH.
staged="${dir}/.salvage.new.$$"
cp "${tmp}/salvage" "$staged" || fail "copy into ${dir} failed"
chmod 0755 "$staged" || { rm -f "$staged"; fail "chmod failed"; }
mv "$staged" "${dir}/salvage" || { rm -f "$staged"; fail "install into ${dir} failed"; }

say ""
say "  installed: ${dir}/salvage  (verified: ${verified})"
say "  $("${dir}/salvage" version)"
case ":${PATH}:" in
  *":${dir}:"*) ;;
  *) say "  note: ${dir} is not on your PATH — add it, e.g.: export PATH=\"${dir}:\$PATH\"" ;;
esac
