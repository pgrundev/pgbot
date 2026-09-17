# Contributing to pgbot

Thanks for helping. pgbot is a read-only PostgreSQL diagnostics CLI; a few
invariants are load-bearing, so please read the short list below before a PR.

## Build and test

```bash
go build ./cmd/pgbot            # static binary (CGO stays off)
go test ./...                  # unit tests; DB-dependent ones self-skip
scripts/gate.sh                # the real gate — builds HEAD, not your working tree
```

`scripts/gate.sh` refuses a dirty tree, then builds and tests the committed HEAD
in an isolated clone across every released target (linux, darwin and windows ×
amd64, arm64). Run it before pushing — a green working-tree `go test` can hide a
partial commit that doesn't compile.

Integration tests run against a real database when `PGBOT_TEST_DSN` (a superuser
DSN unlocks the doc-verify guard via `PGBOT_TEST_SUPERUSER_DSN`) is set:

```bash
docker run -d -p 5432:5432 -e POSTGRES_PASSWORD=pw postgres:18 -c shared_preload_libraries=pg_stat_statements
PGBOT_TEST_SUPERUSER_DSN=postgres://postgres:pw@127.0.0.1:5432/postgres go test ./internal/collect/ -run Integration
```

CI also runs `gofmt -l`, `go vet`, `go test -race`, `golangci-lint`, and
`govulncheck`; keep them green.

## The invariants (please don't break these)

- **Read-only, always.** pgbot pins `default_transaction_read_only`, a 15s
  `statement_timeout`, and a 2s `lock_timeout`, and runs everything inside
  `BEGIN READ ONLY`. It never writes to the target.
- **Never execute an inspected query.** Only plain `EXPLAIN` is allowed —
  `EXPLAIN ANALYZE` (or the `ANALYZE` option in any form) is banned tree-wide and
  a CI grep enforces it. New raw-SQL surfaces must reuse `sanitizeQuery` + the
  READ ONLY transaction.
- **Findings are deterministic.** Every finding is computed in Go from a
  `model.Context` (`internal/findings`). The LLM layer explains findings; it never
  generates them.
- **No PII in a `model.Context`.** `pg_stat_activity` text goes through
  `conn.ScrubQueryText`; connection strings through `conn.RedactConnString`;
  `pg_stat_statements` text is scrubbed too (it stores utility statements verbatim).
- **`--json` is additive.** Add fields with `omitempty`, or bump
  `model.SchemaVersion` and regenerate the schema (`go run ./tools/schemagen`).
- **Every finding has a docs page.** A new finding needs a catalog entry
  (`internal/findings/catalog.go`) and a `docs/findings/<id>.md`; a CI test
  enforces the pairing and that the page's verify query actually runs.

## Releasing

See [docs/release.md](docs/release.md) — tag → GoReleaser → per-channel smoke
jobs, and which secret each distribution channel (npm, Homebrew) needs.

## Commits

Conventional-commit prefixes (`feat:`, `fix:`, `docs:`, `test:`, `chore:`, `ci:`)
keep the changelog readable. Keep changes focused; commit each logical unit.
