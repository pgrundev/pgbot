#!/usr/bin/env bash
# Local release gate. Unlike `go test ./...` in the working tree, this builds and
# tests HEAD in an isolated clone — so a partial commit (some files staged, their
# callers not) is caught here instead of by a red main. That exact failure mode
# left main non-compiling for six pushes; this script exists so it can't recur.
#
# Usage: scripts/gate.sh
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

# (1) A dirty tree is the precondition for "HEAD doesn't build but my tree does."
# Refuse to gate until everything is committed, so the clone below is meaningful.
if [[ -n "$(git status --porcelain)" ]]; then
  echo "✗ working tree is dirty — commit or stash before gating (a gate that reads"
  echo "  uncommitted files cannot detect a partial commit):"
  git status --short
  exit 1
fi

head="$(git rev-parse --short HEAD)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# (2) Build and test from HEAD, isolated from the working tree.
git clone --quiet --local . "$tmp"
pushd "$tmp" >/dev/null
echo "→ gating HEAD ($head) in an isolated clone"
CGO_ENABLED=0 go build ./...
CGO_ENABLED=0 go vet ./...
# CI runs golangci-lint; a gate that skips it reports green on pushes CI will
# reject (ST1003 field naming has done exactly that, twice). Hard-require the
# tool rather than soft-skip — a missing linter is a broken dev setup.
command -v golangci-lint >/dev/null || {
  echo "✗ golangci-lint not installed (brew install golangci-lint) — CI runs it, so the gate must too" >&2
  exit 1
}
golangci-lint run ./...
CGO_ENABLED=0 go test ./...
for goos in linux darwin; do
  for goarch in amd64 arm64; do
    CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build -o /dev/null ./cmd/pgbot
  done
done
popd >/dev/null
echo "✓ HEAD builds, vets, and tests clean (4 arches)"

# (3) "Green" must mean CI, not just local — surface the latest main conclusion.
if command -v gh >/dev/null 2>&1; then
  echo "→ latest CI on main (must be success before reporting a push green):"
  gh run list --branch main --limit 3 2>/dev/null || echo "  (gh unavailable — check CI manually)"
else
  echo "→ gh not installed — verify CI on main manually before reporting green"
fi
