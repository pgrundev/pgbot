#!/bin/sh
# pgbot installer. Detects OS/arch, downloads the release tarball, verifies the
# release — cosign signature on checksums.txt (when cosign is present), then the
# tarball's SHA256 against it — before making anything executable, then installs.
#
#   curl -fsSL https://pgbot.dev/install | sh
#
# Env:
#   PGBOT_VERSION            version to install (default: latest)
#   PGBOT_INSTALL_DIR        install directory (default: /usr/local/bin; no sudo if writable)
#   PGBOT_REQUIRE_SIGNATURE  set to 1 to REQUIRE cosign signature verification
#                            (missing cosign, missing artifacts, or a failed check → hard error)
set -eu

REPO="pgrundev/pgbot"
INSTALL_DIR="${PGBOT_INSTALL_DIR:-/usr/local/bin}"

say()  { printf 'pgbot-install: %s\n' "$1" >&2; }
die()  { say "error: $1"; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$os" in
  linux|darwin) ;;
  *) die "unsupported OS: $os (build from source: go install github.com/$REPO/cmd/pgbot@latest)" ;;
esac

arch="$(uname -m)"
case "$arch" in
  x86_64|amd64) arch="amd64" ;;
  arm64|aarch64) arch="arm64" ;;
  *) die "unsupported architecture: $arch" ;;
esac

have curl || have wget || die "need curl or wget"
have sha256sum || have shasum || die "need sha256sum or shasum"

fetch() { # url dest
  if have curl; then curl -fsSL "$1" -o "$2"; else wget -qO "$2" "$1"; fi
}

version="${PGBOT_VERSION:-}"
# "latest" is a convenience alias, not a real tag — resolve it via the API like an
# empty value. (The GitHub Action passes latest by default.)
if [ -z "$version" ] || [ "$version" = "latest" ]; then
  api="https://api.github.com/repos/$REPO/releases/latest"
  # Buffer the response before parsing: grep -m1 exits at the first match,
  # which hands curl an EPIPE and puts a scary-but-harmless
  # "curl: (23) Failure writing output" onto the installer's output.
  release_json="$(fetch "$api" /dev/stdout)" || release_json=""
  version="$(printf '%s' "$release_json" | grep -m1 '"tag_name"' | cut -d'"' -f4)"
  [ -n "$version" ] || die "could not determine latest version; set PGBOT_VERSION"
fi
ver="${version#v}"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

base="https://github.com/$REPO/releases/download/$version"
tarball="pgbot_${ver}_${os}_${arch}.tar.gz"

say "downloading $tarball ($version)"
fetch "$base/$tarball" "$tmp/$tarball" || die "download failed"
fetch "$base/checksums.txt" "$tmp/checksums.txt" || die "checksums download failed"

# --- signature verification --------------------------------------------------
# The SHA256 check below only proves the tarball matches checksums.txt — but an
# attacker who can replace the tarball can replace checksums.txt alongside it.
# cosign verifies that checksums.txt was signed by pgbot's release workflow
# (keyless, GitHub Actions OIDC), which closes that gap. Do this BEFORE trusting
# checksums.txt for the hash comparison.
require_sig="${PGBOT_REQUIRE_SIGNATURE:-0}"
# Identity of pgbot's release workflow (keyless signing, GitHub Actions OIDC).
# Pin to the release workflow itself, not the whole repo: a bare
# "^https://github.com/<repo>/" would accept a signature minted by ANY workflow
# in the repo that holds id-token: write. The release workflow verifies its own
# output against this exact identity (release.yml), so the installer holds
# itself to the same standard.
# The trailing @ anchors the workflow filename: without it, a workflow named
# release.yml.evil.yaml in the same repo would also match the unanchored regexp.
cert_id_re="^https://github.com/${REPO}/\.github/workflows/release\.yml@"
oidc_issuer="https://token.actions.githubusercontent.com"
if have cosign; then
  if fetch "$base/checksums.txt.cosign.bundle" "$tmp/checksums.txt.cosign.bundle" 2>/dev/null; then
    # Preferred: a self-contained cosign bundle (certificate + signature in one
    # file). Verified with --bundle, so no reliance on the --certificate /
    # --signature flags cosign v3 has deprecated.
    say "verifying signature (cosign bundle)"
    if cosign verify-blob \
         --bundle "$tmp/checksums.txt.cosign.bundle" \
         --certificate-identity-regexp "$cert_id_re" \
         --certificate-oidc-issuer "$oidc_issuer" \
         "$tmp/checksums.txt" >/dev/null 2>&1; then
      say "signature OK"
    else
      die "signature verification FAILED — refusing to install"
    fi
  elif fetch "$base/checksums.txt.sig" "$tmp/checksums.txt.sig" 2>/dev/null &&
       fetch "$base/checksums.txt.pem" "$tmp/checksums.txt.pem" 2>/dev/null; then
    # Fallback: detached certificate + signature. cosign v3 deprecated these flags
    # (still functional); the bundle path above supersedes them once a release
    # publishes checksums.txt.cosign.bundle.
    say "verifying signature (cosign)"
    if cosign verify-blob \
         --certificate "$tmp/checksums.txt.pem" \
         --signature "$tmp/checksums.txt.sig" \
         --certificate-identity-regexp "$cert_id_re" \
         --certificate-oidc-issuer "$oidc_issuer" \
         "$tmp/checksums.txt" >/dev/null 2>&1; then
      say "signature OK"
    else
      die "signature verification FAILED — refusing to install"
    fi
  else
    [ "$require_sig" = "1" ] && die "signature artifacts missing for $version (PGBOT_REQUIRE_SIGNATURE=1)"
    say "warning: signature artifacts not found for $version — proceeding with checksum only"
  fi
elif [ "$require_sig" = "1" ]; then
  die "cosign not found but PGBOT_REQUIRE_SIGNATURE=1 — install it: https://docs.sigstore.dev/cosign/installation"
else
  say "note: cosign not found — skipping signature verification (checksum still enforced)."
  say "      install cosign, or set PGBOT_REQUIRE_SIGNATURE=1, to require it."
fi

say "verifying checksum"
expected="$(grep " $tarball\$" "$tmp/checksums.txt" | awk '{print $1}')"
[ -n "$expected" ] || die "no checksum listed for $tarball"
if have sha256sum; then
  actual="$(sha256sum "$tmp/$tarball" | awk '{print $1}')"
else
  actual="$(shasum -a 256 "$tmp/$tarball" | awk '{print $1}')"
fi
[ "$expected" = "$actual" ] || die "checksum mismatch — refusing to install (expected $expected, got $actual)"

tar -xzf "$tmp/$tarball" -C "$tmp"
[ -f "$tmp/pgbot" ] || die "binary not found in archive"
chmod +x "$tmp/pgbot"

# Create the install dir if it doesn't exist yet — a custom PGBOT_INSTALL_DIR
# like ~/.local/bin often won't. Only fall back to sudo when we genuinely can't
# write there, rather than surprising the user with a password prompt for a
# directory they own.
if mkdir -p "$INSTALL_DIR" 2>/dev/null && [ -w "$INSTALL_DIR" ]; then
  mv "$tmp/pgbot" "$INSTALL_DIR/pgbot"
elif have sudo; then
  say "installing to $INSTALL_DIR (needs sudo)"
  sudo mkdir -p "$INSTALL_DIR"
  sudo mv "$tmp/pgbot" "$INSTALL_DIR/pgbot"
else
  die "$INSTALL_DIR is not writable and sudo is unavailable; set PGBOT_INSTALL_DIR to a writable path"
fi

say "installed pgbot $version to $INSTALL_DIR/pgbot"
"$INSTALL_DIR/pgbot" --version || true

# An older pgbot earlier in PATH (brew, go install) silently shadows this
# install: `pgbot --version` above shows the new binary, but the user's shell
# runs the old one. Detect it and say so, or the first symptom is a fresh
# feature "missing" from a just-installed version.
resolved="$(command -v pgbot 2>/dev/null || true)"
if [ -n "$resolved" ] && [ "$resolved" != "$INSTALL_DIR/pgbot" ]; then
  say ""
  say "WARNING: your shell resolves 'pgbot' to $resolved"
  say "         ($("$resolved" --version 2>/dev/null || echo version unreadable)), which is earlier in PATH"
  say "         than this install. Upgrade or remove it so one binary wins:"
  say "           brew upgrade pgbot        # if it came from Homebrew"
  say "           rm \"$resolved\"            # otherwise"
fi
