#!/usr/bin/env bash
# End-to-end packaging proof for THIS platform (DoD 10): build the real Go binary,
# assemble the npm tree with build.mjs, `npm pack` the wrapper + this platform's
# package, install the tarballs with --ignore-scripts, and run `pgbot --version`
# through the installed wrapper. Proves the install needs no lifecycle scripts and
# that the wrapper resolves + execs the binary. No registry, no publish.
#
# Usage: pack-smoke.sh [manager...]   — default npm; CI passes `npm bun`.
# The tarballs are built once and installed by every manager named, each into
# its own project: a wrapper change that only breaks bun would otherwise pass an
# npm-only smoke unnoticed.
set -euo pipefail
[ $# -gt 0 ] || set -- npm

root="$(cd "$(dirname "$0")/../.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
version="0.0.0-smoke"

# Hard-require rather than soft-skip: a skipped leg reports green for a manager it never ran.
for pm in "$@"; do
  command -v "$pm" >/dev/null || { echo "✗ $pm is not installed — pass only the managers you have; CI passes them all" >&2; exit 1; }
done

goos="$(cd "$root" && go env GOOS)"
goarch="$(cd "$root" && go env GOARCH)"
exe="pgbot"
[ "$goos" = "windows" ] && exe="pgbot.exe"

echo "→ building real binary ($goos/$goarch)"
mkdir -p "$work/dist"
( cd "$root" && CGO_ENABLED=0 go build -o "$work/dist/$exe" ./cmd/pgbot )
cat > "$work/dist/artifacts.json" <<JSON
[{"type":"Binary","path":"$work/dist/$exe","goos":"$goos","goarch":"$goarch"}]
JSON

echo "→ assembling npm tree"
DIST="$work/dist" OUT="$work/staging" node "$root/npm/build.mjs" "$version"

platkey="$(ls "$work/staging/@pgbot")"
echo "→ npm pack ($platkey + wrapper)"
mkdir -p "$work/tarballs"
( cd "$work/staging/@pgbot/$platkey" && npm pack --pack-destination "$work/tarballs" >/dev/null 2>&1 )
( cd "$work/staging/pgbot" && npm pack --pack-destination "$work/tarballs" >/dev/null 2>&1 )

# Install both tarballs into a fresh project with lifecycle scripts disabled. The
# wrapper's other-platform optionalDeps are skipped (not published) — that's the
# point. package.json is written by hand: `bun init` would hit the registry.
install_with() {
  local pm="$1" proj="$work/proj-$1"
  mkdir -p "$proj"
  printf '{"name":"pgbot-smoke","private":true}\n' > "$proj/package.json"
  case "$pm" in
    npm) ( cd "$proj" && npm install --ignore-scripts --no-save --no-audit --no-fund \
             "$work/tarballs"/pgbot-*.tgz >/dev/null 2>&1 ) ;;
    bun) ( cd "$proj" && bun add --ignore-scripts "$work/tarballs"/pgbot-*.tgz >/dev/null 2>&1 ) ;;
    *)   echo "✗ no install recipe for package manager '$pm'" >&2; exit 1 ;;
  esac
  echo "$proj"
}

for pm in "$@"; do
  echo "→ $pm install with --ignore-scripts"
  proj="$(install_with "$pm")"

  echo "→ run pgbot --version through the $pm-installed wrapper"
  out="$("$proj/node_modules/.bin/pgbot" --version)"
  echo "   $out"
  case "$out" in
    "pgbot version"*) echo "✓ pack-smoke OK ($platkey, $pm, --ignore-scripts)";;
    *) echo "✗ unexpected --version output via $pm: $out"; exit 1;;
  esac

  # The .bin shim runs the wrapper under node; `bunx --bun` runs it under bun's
  # runtime, whose node-API compatibility (spawn, signals) can break separately.
  if [ "$pm" = bun ]; then
    echo "→ run the wrapper under bun's runtime (--bun)"
    out="$(bun --bun "$proj/node_modules/@pgbot/cli/bin/pgbot.js" --version)"
    echo "   $out"
    case "$out" in
      "pgbot version"*) echo "✓ wrapper OK under bun runtime";;
      *) echo "✗ unexpected --version output under bun runtime: $out"; exit 1;;
    esac
  fi
done
