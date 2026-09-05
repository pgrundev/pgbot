
<h1 align="center">pgbot</h1>

<p align="center">
  <strong>In-database observability for PostgreSQL.</strong><br>
  One static binary connects read-only, reads Postgres's own statistics views,
  and prints a findings-first health report — plus what changed since last time.<br>
  No agent, no external service, no write privilege anywhere in the path.
</p>

<p align="center">
  <a href="https://github.com/pgrundev/pgbot/actions/workflows/ci.yml"><img alt="CI" src="https://github.com/pgrundev/pgbot/actions/workflows/ci.yml/badge.svg"></a>
  <a href="https://github.com/pgrundev/pgbot/releases/latest"><img alt="Release" src="https://img.shields.io/github/v/release/pgrundev/pgbot"></a>
  <a href="https://pkg.go.dev/github.com/pgrundev/pgbot"><img alt="Go Reference" src="https://pkg.go.dev/badge/github.com/pgrundev/pgbot.svg"></a>
  <a href="https://goreportcard.com/report/github.com/pgrundev/pgbot"><img alt="Go Report Card" src="https://goreportcard.com/badge/github.com/pgrundev/pgbot"></a>
  <a href="LICENSE"><img alt="License: Apache-2.0" src="https://img.shields.io/badge/license-Apache--2.0-blue"></a>
  <img alt="PostgreSQL 14–18" src="https://img.shields.io/badge/postgres-14%E2%80%9318-336791">
</p>

<p align="center">
  <a href="#quickstart">Quickstart</a> ·
  <a href="#install">Install</a> ·
  <a href="#setup--a-read-only-role-with-pg_monitor">Setup</a> ·
  <a href="#commands-and-flags">Commands</a> ·
  <a href="#ci-integration">CI</a> ·
  <a href="#mcp--use-pgbot-as-an-agent-tool">MCP</a> ·
  <a href="#the---json-contract">JSON contract</a> ·
  <a href="#troubleshooting">Troubleshooting</a> ·
  <a href="docs/providers.md">Provider notes</a>
</p>

> **Status: beta.** The `--json` contract is versioned (currently `1.2.0`, JSON
> Schema published in [`schema/`](schema/)) and breaking changes to it are
> treated as breaking changes to the tool. The human-readable report is **not**
> a stable interface — parse `--json`, not the terminal output.

---

## Quickstart

![pgbot inspect — a read-only vital-signs read: headline gauges with a status, then the checks that came back clean](docs/img/dashboard.png)

```sh
curl -fsSL https://pgbot.dev/install | sh
pgbot inspect "postgres://pgbot_ro@host:5432/db"
```

Or set the connection once in the environment and drop the argument — handy for
CI and shells, and it keeps the password out of your history and `ps`:

```sh
export DATABASE_URL="postgres://pgbot_ro@host:5432/db"
pgbot inspect
```

pgbot reads the argument first, then `$DATABASE_URL`, then `$PGBOT_DATABASE_URL`.
(Shell note: `export DATABASE_URL="…"` — no `$` on the left, no spaces around `=`.)

Everything pgbot takes from the environment fits in one block — the connection,
and (only if you want the optional `ask`/`explain` AI layer) one model key:

```sh
export DATABASE_URL="postgres://pgbot_ro:…@host:5432/db?sslmode=require"

# optional, for `pgbot ask` / `pgbot explain` — one of:
export OPENAI_API_KEY=sk-…        # → OpenAI (gpt-4o-mini by default)
export GEMINI_API_KEY=…           # → Google Gemini (AI Studio key)
```

Everything else — `inspect`, `queries`, `indexes`, MCP, CI — is fully
deterministic and needs **no key**; nothing leaves your machine. Provider
pinning and model/endpoint overrides: [the AI layer](#explain--optional-ai-layer).

```
connected · db.example.com · postgres 17.4 · read-only · 6h20m window

Database health: 82/100

CRITICAL
● transaction-id age 1.8B — 84% toward wraparound

WARNING
● orders queries 3.2× slower (8 → 26 ms mean)
● 3 unused indexes consume 18 GB
● connection usage reached 87%

GOOD
● cache hit ratio 99.4%
● replication healthy
● no deadlocks

Details: pgbot inspect --full   ·   Machine-readable: --json
Ask it: pgbot ask "what's wrong?"
```

The default report is a **graded read**: a health score, findings bucketed
CRITICAL / WARNING / NOTE, then a GOOD list naming the healthy subsystems with
their values (a tool that names what it verified reads like a colleague who
looked, not an alarm). `pgbot inspect --full` adds a subsystem status board plus
the section tables and per-finding caveats; focused commands (`indexes`,
`queries`, `tables`, `vacuum`) each drill into one signal; `pgbot ask "…"` and
`pgbot explain` put a plain-language AI reading on top of the same findings.
`--json` is the complete, versioned contract for agents and scripts.

```
$ pgbot ask "what's wrong?"

Your database is mostly healthy.

1 critical issue:
orders queries became 3.2× slower in the last 6 hours.

Likely cause:
sequential scans increased after the orders table grew 18%.

Recommended:
review an index on customer_id + created_at.
```

## Why pgbot

| | |
|---|---|
| **Read-only by role, not by flag** | The guarantee is a `pg_monitor` login role with no write grants. Session pinning (`default_transaction_read_only`, `statement_timeout=15s`, `lock_timeout=2s`) and `BEGIN READ ONLY` are defence in depth on top of it. |
| **It remembers** | Every run writes a local baseline, so from the third run on it tells you *what changed and why it matters* — a query that got slower, a table that started sequential-scanning, an index that stopped being used. |
| **Findings are deterministic** | Every finding is computed in Go from SQL. The optional AI layer explains findings; it never generates them. |
| **Nothing to deploy** | One static binary. No collector, no time-series database, no service to run. |
| **Built for agents** | `--json` is a versioned, PII-free contract; `pgbot mcp` exposes the same findings over the Model Context Protocol, with a skill and a Claude Code plugin on top. |

<details>
<summary><strong>How it compares to pganalyze, PMM, pgwatch</strong></summary>

pgbot is a **point-in-time diagnostic you run**, not a monitoring platform you
operate. If you want dashboards, alerting, long retention, and multi-host
rollups, run pganalyze / Percona PMM / pgwatch — pgbot doesn't replace them.
Reach for pgbot when you want an answer in ten seconds without deploying
anything, when you're triaging a database you don't own, or when an AI agent
needs structured Postgres findings it can reason over.

</details>

## Requirements

- PostgreSQL 14–18 (16–18 fully supported, 14–15 best-effort); collectors
  degrade rather than fail on older feature sets — see
  [Version support](#version-support).
- A login role holding `pg_monitor` — see
  [Setup](#setup--a-read-only-role-with-pg_monitor).
- `pg_stat_statements` for the queries section (optional; pgbot prints the
  provider-specific install steps when it's missing).
- `advise` additionally needs the [hypopg](https://github.com/HypoPG/hypopg)
  extension and PostgreSQL 16+.
- Linux, macOS, Windows — amd64 and arm64.

## See it

**`pgbot inspect --full`** — a subsystem status board (one row per subsystem,
colored ok / warn / fail), followed by the detailed section tables.

![pgbot inspect --full — a box-drawing subsystem status board](docs/img/full.png)

**`pgbot indexes`** — zero-scan indexes with sizes, and the caveat that matters:
on a primary those scan counts are per-node, so a replica may still be using an
index that looks unused here. It tells you what *not* to drop.

![pgbot indexes — zero-scan indexes and what not to drop](docs/img/indexes.png)

**`pgbot queries`** — the top statements from `pg_stat_statements`, ranked by
total execution time (the query quietly eating your database) with a `share`
column for each query's slice of total time. Add `--by-calls` to rank by call
count instead — a cheap query run a million times can outweigh an expensive one
run twice. Transaction-control and session-`SET` noise is filtered out.

```
$ pgbot queries "$DATABASE_URL"
  total  share  calls  mean       query
  4h11m  61.0%  812.4k 18.55 ms   SELECT * FROM orders WHERE user_id = $1 AND …
  22m3s  17.8%  1.3k   1.02 s     SELECT count(*) FROM events WHERE created_at …
  15m2s  12.0%  99.8k  9.04 ms    INSERT INTO audit_log (actor, action, …) VAL …
```

**`pgbot vacuum`** — autovacuum health per table: dead tuples, dead-tuple ratio,
when autovacuum last ran, and a computed `due?` — whether the table's dead tuples
have passed Postgres' default autovacuum trigger (`50 + 20%` of live rows). Rising
dead tuples with `due? yes` and no recent run is autovacuum falling behind, the
early signal for bloat and, eventually, wraparound risk.

```
$ pgbot vacuum "$DATABASE_URL"
  table               live   dead   dead%  last autovacuum  due?
  public.demo_events  42.9k  33.8k  44.1%  4m ago           yes
  public.churny       5.0k   10.0k  66.7%  never            yes
```

**`pgbot tables`** — the largest tables by total size (heap + indexes + TOAST),
each with row count, dead-tuple ratio, and sequential-vs-index scan counts. It's
storage accounting *and* a missing-index radar: a large table with heavy `seq
scans` and few `idx scans` is a likely index candidate.

```
$ pgbot tables "$DATABASE_URL"
  size      rows   dead%  seq scans  idx scans  table
  38.7 GiB  19.7M  8.3%   1.5k       112.3M     public.performance_events
  20.0 GiB  1.3M   10.8%  2.5M       121.6M     public.events        ← 2.5M seq scans
  7.1 GiB   5.6M   0.0%   5.0k       46.6M      public.log_entries
```

**`pgbot ask "why is it slow?"`** — a plain-language reading of the *same*
deterministic findings. It leads with the lock contention and refuses to
recommend dropping the indexes because replication is active — the caveat is
carried into the advice, not lost.

![pgbot ask — an AI reading of pgbot's findings, with caveats carried](docs/img/ask.png)

## Commands and flags

Every command takes the connection the same way — an argument, `$DATABASE_URL`,
or `$PGBOT_DATABASE_URL`.

| Command | What it does |
|---|---|
| `inspect` | the full findings-first health report (`--full` for the section tables) |
| `lint` | schema-only check, safe on an empty CI database (`inspect --profile=schema --no-store`) |
| `init` | generate the read-only role setup SQL — nothing is executed (`--verify` checks an existing role) |
| `diff` | compare two baseline snapshots offline |
| `why` | explain a regression from baseline history: symptom ← mechanism ← antecedent, with numbers and onset times (offline; `--duration 10s` adds a live wait diagnosis) |
| `indexes` · `queries` · `tables` · `vacuum` | drill into one signal |
| `logs` | the server log over SQL — newest entries or `--live` follow (experimental) |
| `waits` | sample where database time goes — wait classes, blockers, contention (experimental) |
| `erd` | the schema as an ER diagram, drawn in the terminal (`--mermaid` for GitHub) |
| `activity` | live sessions right now — PIDs, ages, waits, and what each is running |
| `report` | the full inspection as one self-contained HTML page (`pgbot report > report.html`) |
| `advise` | planner-validated missing-index suggestions (needs hypopg) |
| `ask "…"` · `explain` | a plain-language AI reading of the same deterministic findings |
| `explain-finding <id>` | the catalogue page for a finding, offline |
| `mcp` | serve the findings to an AI agent over MCP |
| `config` · `baselines` | manage `.pgbot.toml` and the local baseline store |

Key `inspect` flags:

| Flag | |
|---|---|
| `--json` · `--format=text\|json\|sarif\|junit\|prometheus` | output format; SARIF uploads to the GitHub Security tab |
| `--fail-on=critical\|warn\|info\|none` | the severity that makes the exit code non-zero (the CI gate) |
| `--profile=full\|schema` | `schema` runs only catalog-derived findings — safe on an empty CI database |
| `--fail-on-new <base.json>` | act only on findings not already in a base report (migration PRs) |
| `--all-databases` | inspect every database in the cluster; cluster-wide findings reported once |
| `--config <path>` | a `.pgbot.toml` for thresholds, severity remaps, and `[[ignore]]` rules |

Exit codes are a scriptable contract: `0` clean · `1` warn · `2` critical · `3`
connection/execution failure · `64` usage error. Suppressed and pre-existing
findings never move them.

## Install

| Method | Command |
|---|---|
| npx (no install) | `npx @pgbot/cli inspect "$DATABASE_URL"` |
| Script (cosign signature + checksum) | `curl -fsSL https://pgbot.dev/install \| sh` |
| Homebrew | `brew install pgrundev/tap/pgbot` |
| Go | `go install github.com/pgrundev/pgbot/cmd/pgbot@latest` |
| Docker | `docker run --rm ghcr.io/pgrundev/pgbot inspect "$DATABASE_URL"` |
| Windows / manual | download the archive for your OS/arch from [Releases](https://github.com/pgrundev/pgbot/releases) (Linux/macOS `.tar.gz`, Windows `.zip`) |

`npx @pgbot/cli` fetches the prebuilt binary for your platform from npm (shipped as
an `optionalDependency`, so only the matching one installs) and runs it — nothing to
install, works with `npm ci --ignore-scripts`. It installs the `pgbot` command
(`npm i -g @pgbot/cli`). The package is **scoped**: the bare name `pgbot` is
blocked by npm's package-name-similarity rule (too close to `got`), so
`npx pgbot` returns `E404` — use `npx @pgbot/cli`.

Homebrew installs from the [`pgrundev/homebrew-tap`](https://github.com/pgrundev/homebrew-tap)
tap; the formula is regenerated by every release and pins the SHA-256 of each
platform's release archive. macOS (Intel/Apple Silicon) and Linux (x86_64/arm64).

Against a **remote/managed database** (RDS, Neon, Supabase, …) the container needs
no special networking — it reaches the host directly. Pass the DSN by
**environment**, not as an argument, so the password stays out of `ps` and your
shell history:

```bash
export DATABASE_URL="postgres://pgbot_ro:…@yourdb.example.com:5432/db?sslmode=require"
docker run --rm -e DATABASE_URL ghcr.io/pgrundev/pgbot inspect
```

Use a `pg_monitor` role, not a superuser. (For a database in a local container,
see [Postgres in Docker](#postgres-in-docker) — the networking differs.)

**What each path verifies.** npm is the *convenient* path: the packages carry
registry integrity hashes and npm **provenance**, a verifiable link to the GitHub
Actions workflow that built them — that attests *where* the package came from, not
that the artifact was signed. `install.sh` is the *verified* path: releases ship
SHA256 checksums signed with **cosign** (keyless, via GitHub Actions OIDC), and the
script verifies that signature when `cosign` is on your `PATH` and always verifies
the checksum. For the strongest guarantee, require the signature:

```bash
PGBOT_REQUIRE_SIGNATURE=1 curl -fsSL https://pgbot.dev/install | sh
```

`PGBOT_REQUIRE_SIGNATURE=1` hard-fails if `cosign` is missing or the check doesn't
pass. To verify a release by hand:

```bash
cosign verify-blob --bundle checksums.txt.cosign.bundle \
  --certificate-identity-regexp '^https://github.com/pgrundev/pgbot/\.github/workflows/release\.yml@' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com checksums.txt
```

<details>
<summary><strong>Uninstalling</strong></summary>

```bash
rm "$(command -v pgbot)"
rm -rf "${XDG_STATE_HOME:-$HOME/.local/state}/pgbot"   # local baseline store
```

pgbot writes nothing else on your machine — no daemons, no launch agents — and
nothing in the database: no extensions, no tables, no roles.

</details>

## Setup — a read-only role with `pg_monitor`

The read-only guarantee is **the role**, not a flag. Create a login role that
holds `pg_monitor` (so it can see the full statistics views) and has no write
grants:

```sql
CREATE ROLE pgbot_ro LOGIN PASSWORD '...';
GRANT pg_monitor TO pgbot_ro;
GRANT CONNECT ON DATABASE yourdb TO pgbot_ro;
```

Or have `pgbot init` write exactly this for you — tailored to your provider and
database name, including the provider-specific `pg_stat_statements` step.
pgbot itself executes none of it (pgbot never writes); you review and run it:

```sh
pgbot init "postgres://admin@host:5432/db" | psql "postgres://admin@host:5432/db"
pgbot init --verify "postgres://pgbot_ro:…@host:5432/db"   # confirm the role works
```

Without `pg_monitor`, a non-superuser sees only its own sessions in
`pg_stat_activity` and can't read several views fully — pgbot detects this at
connect time and tells you exactly which GRANT to run rather than silently
reporting partial data.

pgbot additionally pins every session read-only (`default_transaction_read_only`,
`statement_timeout=15s`, `lock_timeout=2s`) and wraps each query in its own
`BEGIN READ ONLY … COMMIT`. It **commits** those read-only probes rather than
rolling them back — a read-only transaction writes nothing either way, but a
rollback would inflate the `xact_rollback` counter pgbot itself reports. Those
are defence in depth; the role is the boundary.

### What it costs your database

A run opens **one connection pool capped at 4 connections**, holds no long
transactions (every probe is its own `BEGIN READ ONLY … COMMIT` under the
pinned `statement_timeout=15s` / `lock_timeout=2s`), and takes no locks beyond
the shared catalog access any `SELECT` takes. Counters are sampled twice across
the `--interval` gap (default 1s), so a full `inspect` finishes in a few
seconds of wall clock. It is safe to run against a busy primary — pgbot even
excludes its own sessions, transactions, and temp usage from what it reports,
so it never measures its own footprint as the database's. Run it against a
replica if you prefer, noting the per-node index-scan caveat in
[`pgbot indexes`](#see-it).

## Point pgbot at your database

Pass the connection string as an argument — a URL or a libpq DSN:

```bash
pgbot inspect "postgres://pgbot_ro:secret@host:5432/db?sslmode=require"

# the libpq keyword/value DSN form works too:
pgbot inspect "host=host port=5432 dbname=db user=pgbot_ro sslmode=require"
```

Or set it once in the environment and omit the argument — convenient for a shell
session or CI, and it keeps the password out of your shell history and `ps`
output:

```bash
export DATABASE_URL="postgres://pgbot_ro:secret@host:5432/db?sslmode=require"

pgbot inspect
pgbot queries     # every command takes the connection the same way
pgbot diff --since 24h
```

pgbot resolves the connection in this order: the argument first, then
`$DATABASE_URL`, then `$PGBOT_DATABASE_URL`. Add `?sslmode=require` (or stricter)
for any database reached over a network.

### Environment reference

| Variable | Purpose |
|---|---|
| `DATABASE_URL` / `PGBOT_DATABASE_URL` | Connection used when no connection string is passed (checked in that order, after the argument). |
| `NO_COLOR` | Disables ANSI output (as does a non-TTY, or `--no-color`). |
| `XDG_STATE_HOME` | Where the baseline store lives; defaults to `~/.local/state`. |
| `PGBOT_CONFIG` | Path to `.pgbot.toml` (otherwise discovered from cwd upward, then `$XDG_CONFIG_HOME`). |
| `OPENAI_API_KEY` | Enables `ask` / `explain` via OpenAI. Keys are never accepted as flags. |
| `GEMINI_API_KEY` / `GOOGLE_API_KEY` | Enables `ask` / `explain` via Google Gemini. |
| `PGBOT_AI_PROVIDER` | Forces `openai` or `gemini` when both keys are set. |
| `PGBOT_OPENAI_MODEL` / `PGBOT_OPENAI_URL` | Model/endpoint override (any OpenAI-compatible endpoint works). |
| `PGBOT_GEMINI_MODEL` / `PGBOT_GEMINI_URL` | Model/endpoint override for Gemini. |
| `PGBOT_REQUIRE_SIGNATURE` | `install.sh` only: hard-fail unless the cosign signature verifies. |

## Connecting to managed providers

pgbot is a **client** — it connects over the Postgres wire protocol like `psql`.
You never install anything on the database; run pgbot from your laptop, a bastion,
CI, or an instance in the same network. Grant `pg_monitor` to your role (above)
and connect. Provider-specific notes:

### AWS RDS / Aurora

You can't install on the RDS/Aurora instance itself — it's managed, no OS access.
Run pgbot from a **client that can reach it**:

- **Private RDS (recommended for prod):** run pgbot from a small **EC2 in the same
  VPC**. It reaches the private endpoint over AWS's internal network — no public
  access, no SSH tunnel, no IP allow-listing. The only rule is the RDS security
  group allowing `5432` from the EC2's security group.
- **Publicly accessible RDS:** allow your IP in the RDS security group and connect
  straight from your laptop.

```bash
# on the EC2 (or your laptop for a public instance):
curl -fsSL https://pgbot.dev/install | sh
pgbot inspect "postgres://pgbot_ro@mydb.abc123.us-east-1.rds.amazonaws.com:5432/appdb?sslmode=require"
```
Grant `pg_monitor` as the master (`rds_superuser`) role. **Caveat:** host metrics
(CPU / memory / disk IOPS) live in CloudWatch, not Postgres, so they're out of
reach over a connection string — everything else works.

### Neon

```bash
pgbot inspect "postgres://user:pass@ep-xxx.region.aws.neon.tech/dbname?sslmode=require"
```
- The **pooled** endpoint has a `-pooler` host suffix (transaction mode). pgbot
  detects it and proceeds — rates stay correct — or use the direct (non-pooler)
  host for session-scoped certainty.
- Neon's default string ships `channel_binding=require`; pgbot **ignores it
  automatically** (the driver can't do channel binding; TLS from `sslmode` still
  applies) instead of erroring.
- `pg_stat_statements` is preloaded — just `CREATE EXTENSION pg_stat_statements;`.
- **Scale-to-zero:** after idle, Neon suspends the compute and discards stats. The
  first run after a wake is a *cold window* — pgbot suppresses counter-based
  findings until the window is old enough, so a reset never reads as a −99% regression.

### Supabase

```bash
# direct endpoint (session-scoped, best for pgbot):
pgbot inspect "postgres://postgres:pass@db.<ref>.supabase.co:5432/postgres?sslmode=require"
# or the pooled endpoint (:6543, transaction mode) — pgbot notes it and proceeds:
pgbot inspect "postgres://postgres.<ref>:pass@aws-0-<region>.pooler.supabase.com:6543/postgres?sslmode=require"
```
- The default pooled connection string uses port **`:6543`** (Supavisor, transaction
  mode). pgbot detects the pooler and proceeds with a note; prefer the direct
  `:5432` endpoint when you can.
- `pg_stat_statements` is preloaded — `CREATE EXTENSION pg_stat_statements;`.
- Supabase doesn't hand out superuser; the built-in `postgres` role already has
  broad read access, or grant `pg_monitor` to a dedicated role where allowed.

### Postgres in Docker

The connection string depends on **where pgbot runs relative to the container.**

**pgbot on the host, container with a published port.** Read the `PORTS` column of
`docker ps` — `0.0.0.0:6433->5432/tcp` means host port `6433` maps to the
container's `5432`. Connect to the **host** port:

```bash
docker port mypg 5432                    # → 0.0.0.0:6433  (find the host port)
pgbot inspect "postgres://postgres:pw@127.0.0.1:6433/postgres?sslmode=disable"
```

Use `127.0.0.1`, not `localhost`: `localhost` resolves to IPv6 (`::1`) first, which
Docker Desktop doesn't forward, so the connect stalls ~10s before falling back to
IPv4. Local containers usually have no TLS → `sslmode=disable`. Find the
credentials with `docker exec mypg env | grep POSTGRES`.

**pgbot as a container reaching a DB container.** `localhost` would mean pgbot's
own container — join the DB's network and use the **container name** + internal
port `5432`:

```bash
docker run --rm --network <that-network> ghcr.io/pgrundev/pgbot \
  inspect "postgres://postgres:pw@mypg:5432/postgres?sslmode=disable"
```

The image is multi-arch (amd64/arm64) and public — no login needed. Prefer passing
the DSN by environment so it stays out of the container's argument list:

```bash
docker run --rm --network <that-network> -e DATABASE_URL ghcr.io/pgrundev/pgbot inspect
```

**pgbot as a container reaching a DB on the host.** Use `host.docker.internal`
(add `--add-host=host.docker.internal:host-gateway` on Linux).

> Rule of thumb: same-network containers address each other by **container name +
> internal port `5432`**; the host reaches a container by **`127.0.0.1` + the
> published host port**. A container with no `->` mapping in `docker ps` isn't
> reachable from the host at all — publish it with `-p`, or connect from inside
> its network.

## Usage

```
pgbot inspect <connection-string>   # URL or libpq DSN, or set $DATABASE_URL
  --json                 emit the versioned, PII-free Context (the agent/script contract)
  --interval 1s          gap between the two counter samples (min 500ms)
  --no-store             don't read or write the local baseline
  --no-color             disable ANSI (also honors NO_COLOR and non-TTY)

pgbot baselines list                # what's stored locally, per database
pgbot baselines prune <fingerprint> # delete a database's snapshots
pgbot baselines export <fingerprint># dump stored snapshots as JSON

pgbot indexes <connection-string>   # zero-scan indexes + what NOT to drop
  --correlate            grade each index (catalog_proven/needs_code_check/inconclusive) + what to grep in code
pgbot queries <connection-string>   # top pg_stat_statements by total time (--by-calls to re-rank)
pgbot tables  <connection-string>   # largest tables + row counts + seq-vs-index scan pattern
pgbot vacuum <connection-string>    # autovacuum health per table — dead tuples + whether it's due
pgbot tune <connection-string>      # config-tuning recommendations from the workload
pgbot explain <connection-string>   # inspect, then have an AI explain the findings
pgbot ask "why is it slow?"         # AI answer grounded on the findings ($DATABASE_URL)
  --yes                  skip the "this sends data to Google" confirmation
pgbot mcp                           # run as an MCP server over stdio (for AI agents)
```

### MCP — use pgbot as an agent tool

`pgbot mcp` speaks the [Model Context Protocol](https://modelcontextprotocol.io)
on stdio, so an AI agent can call pgbot as a read-only tool. It exposes
**deterministic** tools only and lets the *connected model* do the explaining:

- `inspect` — full findings as JSON
- `unused_indexes`, `top_queries`, `vacuum_health` — the CLI's focused views
- `suggest_indexes` — planner-validated index recommendations (hypopg)
- `index_code_correlation` — grades each unused/redundant/invalid index
  (`catalog_proven` / `needs_code_check` / `inconclusive`) and, for the actionable
  ones, the exact identifiers to grep (all case conventions) with the instruction
  to search *filters* (WHERE/JOIN/ORDER BY), never SELECT lists. pgbot never reads
  your repo; it says what to search for. Also `pgbot indexes --correlate [--json]`.
- `record_index_verdict` — store what the repo search found, so a later run over a
  longer window carries strengthening evidence (no DB connection needed)
- `explain_plan` — the planner's plan for a SELECT (plain EXPLAIN, never executed)
- `schema_of` — a table's columns/indexes/constraints + row estimate, **no data**
- `compare_to_baseline` — the `diff`, with its interval-honesty and reset caveats
- `why` — the causal chains from stored history (symptom ← mechanism ←
  antecedent), computed offline from the local store
- `explain_finding` — pgbot's catalogue page for a finding, so the agent explains
  a recommendation in pgbot's words instead of inventing them

Every tool is read-only, returns a stable JSON shape carrying its `exactness`
label, honors `.pgbot.toml` suppression, and never exposes a raw connection
string or query literals to the model. The agent reasons over the same findings
the CLI computes.

In Claude Code it's one line:

```sh
claude mcp add pgbot --env DATABASE_URL="postgres://pgbot_ro:…@host:5432/db?sslmode=require" -- pgbot mcp
```

Or add it to any MCP client's config (Claude Desktop, Cursor, …):

```json
{
  "mcpServers": {
    "pgbot": {
      "command": "pgbot",
      "args": ["mcp"],
      "env": { "DATABASE_URL": "postgres://pgbot_ro@host:5432/db" }
    }
  }
}
```

With `DATABASE_URL` set, the agent calls `inspect` with no arguments; or it can
pass `connection_string` per call to reach several databases. pgbot never writes,
so there's nothing an agent can break through it.

It also exposes a **`diagnose` prompt** (a one-click "inspect and give me a
prioritized diagnosis" workflow) and a **`pgbot://baselines` resource** (the
databases pgbot has local history for) — so tools, prompts, and resources are all
available to the agent.

**Pair it with the skill.** MCP gives the agent the *tools*; the
[`postgres-diagnostics` skill](skills/postgres-diagnostics/SKILL.md) gives it the
*playbook* — respect caveats, never `EXPLAIN ANALYZE`, prioritize by impact,
never write. One command installs it into Claude Code, Cursor, or Codex:

```bash
npx skills add pgrundev/pgbot
```

(or `curl -fsSL https://pgbot.dev/skill | sh` — see [`skills/`](skills/)), and
your agent asks the right pgbot command and reads the results the way pgbot
intends.

### Claude Code plugin

[Claude Code](https://claude.com/claude-code) users can install the tools, the
skill, and the commands in one shot — the repo is its own plugin marketplace:

```bash
claude plugin marketplace add pgrundev/pgbot
claude plugin install pgbot@pgbot
```

That registers the pgbot **MCP tools**, the **`postgres-diagnostics` skill**, and
three slash commands — **`/pg-health`**, **`/pg-slow`**, **`/pg-indexes`** — each
of which carries the pgbot judgment (caveats intact, impact-first, never writes).
The plugin drives the `pgbot` binary, so install that first (`curl -fsSL
https://pgbot.dev/install | sh`); set `DATABASE_URL` or pass a connection string
per call, then ask *"is my Postgres healthy?"*

### `explain` — optional AI layer

`pgbot explain` runs the exact same read-only inspection, prints the
deterministic report unchanged, then asks a model to **explain and prioritize**
the findings in plain language. The findings are still computed locally in Go —
the model only interprets them, it never invents them, and it's instructed to
carry every caveat into any recommendation. The AI text is printed below a
labeled rule (`🤖 generated by … — verify before acting`); if the model errors
or the key is unset, the deterministic report still stands.

With a remote model, this sends the same PII-free Context shown by
`inspect --json`. Before sending it, pgbot identifies the provider, host, and
model and asks for confirmation. Local endpoints are identified as local and do
not require confirmation.

| Provider | Key | Default model | API |
|---|---|---|---|
| Gemini | `GEMINI_API_KEY` / `GOOGLE_API_KEY` | `gemini-flash-latest` | `generateContent` |
| Anthropic | `ANTHROPIC_API_KEY` | `claude-opus-5` | `/v1/messages` |
| OpenAI | `OPENAI_API_KEY` | `gpt-5.6-terra` | `/chat/completions` |
| xAI | `XAI_API_KEY` / `GROK_API_KEY` | `grok-4.6` | `/responses` |

The OpenAI provider also supports compatible services such as OpenRouter,
Groq, Together, DeepSeek, Mistral, Ollama, vLLM, and LM Studio.

```
export OPENAI_API_KEY=…
pgbot explain "$DATABASE_URL"
```

Use `PGBOT_AI_PROVIDER` to select a provider explicitly. `PGBOT_AI_MODEL`,
`PGBOT_AI_BASE_URL`, `PGBOT_AI_API_KEY`, and `PGBOT_AI_REASONING_EFFORT`
override its defaults. Existing `PGBOT_GEMINI_MODEL` and `PGBOT_GEMINI_URL`
and `PGBOT_OPENAI_MODEL` and `PGBOT_OPENAI_URL` settings remain supported. Keys
are read only from environment variables.

**Exit codes** (a stable contract for CI): `0` clean · `1` warnings · `2` critical
findings · `3` connection/execution failure · `64` usage error (bad flags/args).
Suppressed findings never contribute to the exit code.

### `advise` — index suggestions the planner validates

`pgbot advise` finds missing indexes without guessing. It reads the slowest
queries from `pg_stat_statements`, derives candidate indexes from the planner's
own sequential-scan filters (deterministically, in Go — never an LLM), and then
**validates each one**: it creates the index *hypothetically* with
[hypopg](https://github.com/HypoPG/hypopg), re-plans the query, and only reports
it if the planner actually switches to it and the estimated cost drops.

```
$ pgbot advise "$DATABASE_URL"
index advisor · app · postgres 17 · hypopg validation — nothing was built

1 validated recommendation(s):

⚑ public.orders
  CREATE INDEX ON public.orders (customer_id, status);
  helps: SELECT count(*) FROM orders WHERE customer_id = $1 AND status = $2
         60 calls · 68% of DB time
  planner confirmed: cost 4653 → 4.1 (−99.9%)
  ↳ nothing was created. Review, then build off-peak: add CONCURRENTLY.
```

Nothing is ever built — the hypothetical indexes live in backend memory and are
discarded. Everything runs in a READ ONLY transaction; pgbot only *plans* your
query (`EXPLAIN (GENERIC_PLAN)`), it never executes it and never uses the
executing form of EXPLAIN. Requires **hypopg**, **pg_stat_statements**, and
**PostgreSQL 16+**; when any is missing it prints exactly what to enable and does
nothing else. `--json` gives structured recommendations for agents (also exposed
as the MCP `suggest_indexes` tool).

> **Local Docker gotcha:** with a database in Docker Desktop, connect via
> `127.0.0.1`, not `localhost`. `localhost` resolves to IPv6 (`::1`) first, which
> Docker Desktop doesn't forward, so the connect stalls for ~10s before falling
> back to IPv4. Managed hosts (RDS, Supabase, Neon…) aren't affected.

The baseline store lives at `$XDG_STATE_HOME/pgbot/baselines.db` (7 days at full
resolution, hourly rollups to 90 days, 100 MB cap). It's yours — inspect and
delete it with `pgbot baselines`.

## What it collects

All from SQL — connections, cache-hit ratio, TPS and rollback ratio, WAL and IO
rates, checkpoints, locks and blocking chains, replication lag, replication-slot
WAL retention and logical-subscription health, top queries
(`pg_stat_statements`), table/index sizes, dead tuples and vacuum activity,
unused and missing indexes, and non-default settings. Counters
(`pg_stat_database`, `pg_stat_wal`, IO) are **double-sampled** to produce live
rates; the rest are point-in-time reads trended against the baseline.

## The `--json` contract

`--json` (and `--format=json`) is the interface to build on — a versioned,
PII-free document (`schema_version`, currently `1.2.0`) whose machine-checkable
JSON Schema is published in [`schema/`](schema/). Every section carries an
`exactness` label — `sampled`, `cumulative`, `scraped`, or `unavailable` — so a
consumer never mistakes a cumulative total for a live rate.

Versioning policy: additive fields bump the minor version and are not breaking —
a `1.1.0` consumer parses `1.2.0` output unchanged; breaking changes to the
contract are treated as breaking changes to the tool. `pgbot advise --json` has
its own schema
([`schema/pgbot-advise-1.0.0.json`](schema/pgbot-advise-1.0.0.json)).

## Version support

Collectors degrade rather than fail when a capability is absent:

| Feature | From | Fallback |
|---|---|---|
| `pg_stat_wal` (WAL rates) | PG 14 | section marked unavailable |
| `pg_stat_io` (buffers written) | PG 16 | `pg_stat_bgwriter` |
| `pg_stat_checkpointer` | PG 17 | `pg_stat_bgwriter` |
| `stats_fetch_consistency` | PG 15 | separate per-sample transactions |
| `pg_stat_statements` | extension | queries section unavailable + install hint |

### Supported versions

| Tier | Versions | In CI |
|---|---|---|
| **Supported** | PostgreSQL 16, 17, 18 | every PR + push |
| **Best-effort** | PostgreSQL 14, 15 | every PR + push |
| Unsupported | PostgreSQL 13 and older | — (13 is [end-of-life](https://www.postgresql.org/support/versioning/)) |

New features may target 16+ without a backward path. Everything degrades rather
than errors on an older or capability-limited server (see the table above).

### Managed providers

pgbot detects the platform (RDS, Aurora, Cloud SQL, Azure Flexible Server,
Supabase, Neon) and prints the provider-specific steps to enable
`pg_stat_statements` when it's missing. Supabase (`:6543`) and Neon (`-pooler`)
default to a pooled endpoint, which pgbot notes without degrading its rates;
Neon's scale-to-zero discards stats, which pgbot handles as a cold window. Full
per-provider notes and the live-verification checklist are in
[`docs/providers.md`](docs/providers.md).

## Configuration & suppression

An optional `.pgbot.toml` (committed to your repo) overrides thresholds, remaps a
finding's severity, and suppresses specific findings so noise never trains people
to ignore the severity column:

```toml
schema = 1
[severity]
checksums_disabled = "info"        # can't change it on this managed provider
[[ignore]]
finding = "unused_indexes"
object  = "public.idx_legacy_*"    # glob; omit to mute the whole finding
reason  = "backs the quarterly export"
expires = "2026-12-31"
```

Suppression is always **visible** — suppressed findings stay in `--json` (with
`suppressed`/`suppression_reason`), never affect the exit code, and a suppressed
**critical still renders** (a config must not hide `checksum_failures`). pgbot
refuses to read any credential-shaped key from the file, flags rules that have
gone stale, and ships `pgbot config check` / `explain` / `init`. Full contract —
including the per-finding object-identity table — in
[`docs/configuration.md`](docs/configuration.md).

### `diff` — what changed since last time

`pgbot diff [--since 24h]` compares the two most relevant baseline snapshots from
the local store — no connection needed. It's honest about what it compared:

```
$ pgbot diff --since 24h
diff · prod · a1b2c3d4e5f6
2026-08-16 09:00 → 2026-08-17 16:00  ·  31h elapsed
note: you asked for ~24h back, but the nearest older snapshot is 31h back — comparing that.
```

It prints the interval it *actually* used (the nearest snapshot to `--since`, not
a silent substitution), warns up front when a **stats reset** or
**pg_stat_statements eviction** between the snapshots makes specific deltas
untrustworthy, and refuses to compare two different databases (pass
`--fingerprint` when the store holds more than one).

### `why --duration` — live diagnosis: executing, or waiting?

```sh
pgbot why --duration 10s          # sample the live database, then diagnose
```

`why` stays fully offline by default. With `--duration` it also runs a wait
study (the same engine as `pgbot waits`) and classifies the result through a
deterministic, first-match cause table: **transaction lock contention** (only
with a sustained, named blocker), **storage/WAL wait** (IO alone never claims
a missing index), **client/application wait** (explicitly not a PostgreSQL
problem), **saturated with active work**, **not significant** (waits on
near-zero activity are noise), or **insufficient evidence** — refusing to
conclude beats a confident guess. When the local store holds wait history, a
labeled ratio corroborates: *"Lock waits 8× vs the previous 24h."* Shares are
sampled; the only exact numbers are ages read from the server.

### `erd` — the schema, as a diagram, in your terminal

```sh
pgbot erd                # box-drawn tables + a crow's-foot relationship forest
pgbot erd --schema app   # one schema only
pgbot erd --layout row   # left-to-right, dashed ascii edges
pgbot erd --mermaid      # erDiagram text — pasteable into GitHub or mermaid.live
pgbot erd --html > schema.html   # self-contained interactive SVG: pan, zoom, share
```

Structure only — tables, columns, keys, foreign-key edges from `pg_catalog` —
never data, and the connection string never leaves your machine (unlike
paste-your-DSN diagram websites). Needs nothing beyond CONNECT. The `--html`
file is fully self-contained — inline SVG and inline script, zero external
requests — so the schema never leaves the file either.

### `waits` — where database time goes (experimental)

```sh
pgbot waits                         # sample 10s: wait classes, top events, blockers
pgbot waits --duration 30s --group query
pgbot waits --pid 18442             # one backend's story
pgbot waits --json                  # versioned pgbot-waits document (scrubbed)
```

Samples `pg_stat_activity` at 10 Hz and the lock graph (`pg_blocking_pids`)
at 1 Hz for a bounded window — no extension, no eBPF, no daemon, plain
`pg_monitor`. Reports average active sessions, DB time by wait class, top wait
events, waiting sessions, and **blockers with evidence**: a holder is named
only when seen across several lock snapshots (one glimpse is listed as
transient, never blamed). Every share is labeled *sampled* — the only exact
numbers are ages read from the server — and lock contention is explicitly
called out as *not* evidence of a missing index. Aggregated wait counts fold
into the local baseline store (`--no-store` skips), so `diff` and `why` gain
wait history over time.

### `logs` — the server log, over SQL (experimental)

```sh
pgbot logs               # the newest 100 entries, typed: query / info / warn / error
pgbot logs --last 20     # fewer (or more)
pgbot logs --live        # keep following — Ctrl+C to stop (--follow / -f works too)
pgbot logs --level warn,error --json   # scrubbed NDJSON for scripts and agents
```

No agent and no file access: pgbot reads the server's own logfile through
`pg_current_logfile()` and `pg_read_binary_file()`, whichever format it writes
(jsonlog, csvlog, or stderr with any `log_line_prefix`), and follows rotation.
pgbot's own footprint — its probe, its polling reads — is filtered out of the
stream, so a live tail never reads its own reads, and `--last 100` means 100
entries of *your* activity. The human output shows log lines verbatim; `--json`
passes every message through the same literal scrubber as the rest of pgbot.

Reading log content is the one thing `pg_monitor` cannot do, so this command
needs a single extra grant, printed exactly when it's missing:

```sql
GRANT EXECUTE ON FUNCTION pg_read_binary_file(text, bigint, bigint, boolean) TO pgbot_ro;
```

(`pgbot init` includes it commented out; `pgbot init --logs` emits it active.)

A server without a log collector (`logging_collector=off` — the Docker image
default) has no file to read; pgbot says so and points you at `docker logs`.
Managed providers that keep logs behind their own APIs (Neon, Supabase) are out
of reach over SQL — that's their boundary, not a pgbot flag away.

> **Whole cluster:** `pgbot inspect "$DATABASE_URL" --all-databases` inspects every
> connectable database on the server. Cluster-wide findings (settings, replication,
> archiving, wraparound) are reported once; per-database findings appear per
> database. Serial by default (`--parallel N` to fan out).

## CI integration

pgbot is built to run in a pipeline. `--fail-on` decouples the exit code from the
default severity map, and `--format` emits machine-readable reports:

```bash
pgbot inspect "$DATABASE_URL" --fail-on=critical --format=sarif > pgbot.sarif
```

`--format=sarif` produces [SARIF 2.1.0](https://sarifweb.azurewebsites.net/);
upload it with `github/codeql-action/upload-sarif` and every finding lands in your
repo's **Security** tab, linked to its catalogue page. `--format=junit` feeds
Jenkins/GitLab test panes. Suppressed findings stay visible (a SARIF suppression /
a JUnit `skipped`) and never affect the exit code.

### GitHub Action

```yaml
- uses: pgrundev/pgbot@v1
  with:
    dsn: ${{ secrets.PGBOT_DSN }}
    fail-on: critical
```

That runs the check and uploads SARIF to the Security tab. **The DSN must be a
`pg_monitor` role with no data access — never a superuser.** Create one:

```sql
CREATE ROLE pgbot_ci LOGIN PASSWORD '…';
GRANT pg_monitor TO pgbot_ci;
GRANT CONNECT ON DATABASE yourdb TO pgbot_ci;
```

`pg_monitor` grants read access to the statistics views pgbot needs and nothing
else — no table data. The job that uploads SARIF needs `security-events: write`.

### Reviewing a migration PR: schema profile + fail-on-new

An empty CI database has never been queried, so the full profile fires
`unused_indexes` and `stale_statistics` on everything and buries the one change
that matters. `--profile=schema` runs only the findings derivable from the catalog
(invalid/redundant indexes, unindexed FKs, a narrow `int4`/`serial` identity
column, autovacuum disabled on a table) — valid on an empty, freshly-migrated
database. Pair it with `--fail-on-new` so the check fails **only on what the PR
introduced**, not pre-existing findings:

```yaml
# .github/workflows/pr.yml — runs on every pull request
- run: |                      # 1. base branch, migrated → base report
    git checkout ${{ github.base_ref }} && ./migrate.sh
- uses: pgrundev/pgbot@v1
  with: { dsn: "${{ env.CI_DSN }}", profile: schema, format: json }
- run: mv pgbot-report.json base.json
- run: |                      # 2. PR branch, migrated
    git checkout ${{ github.sha }} && ./migrate.sh
- uses: pgrundev/pgbot@v1     # fails only on findings new vs base
  with: { dsn: "${{ env.CI_DSN }}", profile: schema, base-report: base.json, fail-on: warn }
```

(Or skip step 1 and download a `base.json` artifact your `main` CI already
produced.) This is deliberately **quiet when correct** — no PR comment, SARIF
annotations for new findings only, and the exit code carries the verdict. A check
that speaks on every PR is a check nobody reads.

The two profiles answer different questions and you want both: the schema check on
`pull_request` above, and the **full profile against production on a schedule** —

```yaml
# .github/workflows/nightly.yml
on: { schedule: [{ cron: "0 7 * * *" }] }
# ... uses: pgrundev/pgbot@v1 with a read-only DSN to the production replica
```

— which sees backups, replication, bloat, and wraparound that a schema check
never can. A clean `--profile=schema` report says nothing about a running
database's health; its own header says so.

### Prometheus

`--format=prometheus` writes the [node_exporter textfile
format](https://github.com/prometheus/node_exporter#textfile-collector): every
finding as a `pgbot_finding{id,severity,dimension,object}` series plus the gauges
behind them (`pgbot_cache_hit_ratio`, `pgbot_xid_age`, `pgbot_connections_used`,
`pgbot_replica_lag_seconds`, …), so an alert can fire on a trend before a finding
crosses its threshold. Suppressed findings are exported with `suppressed="true"`,
not dropped — a muted config stays visible in your metrics.

pgbot has **no daemon** — that is deliberate. Point it at a textfile collector on a
cron or systemd timer:

```bash
pgbot inspect "$DATABASE_URL" --format=prometheus > /var/lib/node_exporter/pgbot.prom.$$
mv /var/lib/node_exporter/pgbot.prom.$$ /var/lib/node_exporter/pgbot.prom   # atomic
```

Under `--all-databases`, each database's series carry a `database="…"` label.

## The findings catalogue

Every finding pgbot emits has a reference page — what it observed, why it matters,
a **read-only query to verify it yourself**, how to fix it, when to ignore it (with
a pasteable `[[ignore]]` block), and what pgbot cannot see. Browse them by symptom
in [`docs/findings/`](docs/findings/README.md), or read one offline straight from
the binary:

```
pgbot explain-finding low_hot_update_ratio
```

Every line of a report tells you its id, so `pgbot explain-finding <id>` always
has the page.

## Serverless Postgres (Neon, scale-to-zero)

Scale-to-zero databases (Neon, Databricks Lakebase, and similar) **discard
in-memory statistics when the compute suspends** — by default after ~5 minutes
idle. After each wake, `pg_stat_statements` history, cache-hit counters and
index-scan counts all start again from zero.

pgbot detects this and **degrades rather than lies**:

- If the statistics were reset (or the server restarted) since the last run, the
  entire `deltas` section is suppressed with a reason — a counter going from 40M
  to 12k is a wake, not a −99.97% change.
- On a cold window (younger than 15 minutes), counter-based findings — unused
  indexes, cache-hit, sequential-scan-heavy — are suppressed, because they'd be
  meaningless or actively dangerous. Gauges (blocking chains, idle-in-transaction,
  replication lag, invalid indexes) are valid immediately and still reported.
- The report header states the window age plainly.

If you want continuous history, disable scale-to-zero or raise the suspend
timeout so the statistics survive between runs.

## Roadmap and non-goals

- **Host OS metrics** (CPU, disk IOPS, free memory) are **not** reachable over a
  SQL connection. On managed databases they live behind the provider's own API;
  on your own hardware, a future agent-on-host will read them.
- **AI is optional and explain-only.** `pgbot explain` can put a plain-language
  explanation on top of the findings (see above), but the findings themselves are
  always computed deterministically in Go — no model ever generates one. The same
  holds for `pgbot why`: its causal chains are computed from your stored snapshot
  history, never guessed.
- **pgbot never writes.** It recommends indexes; it doesn't create them.

## Troubleshooting

<details>
<summary><strong>Connecting to a local Docker database stalls for ~10 seconds</strong></summary>

Use `127.0.0.1`, not `localhost`. `localhost` resolves to IPv6 (`::1`) first,
which Docker Desktop doesn't forward, so the connect stalls before falling back
to IPv4. Managed hosts (RDS, Supabase, Neon…) aren't affected. See
[Postgres in Docker](#postgres-in-docker).

</details>

<details>
<summary><strong>"queries section unavailable"</strong></summary>

`pg_stat_statements` isn't installed or isn't in `shared_preload_libraries`.
pgbot prints the steps for your specific provider; see
[`docs/providers.md`](docs/providers.md).

</details>

<details>
<summary><strong>Findings look partial or sessions are missing</strong></summary>

The role is missing `pg_monitor`. See
[Setup](#setup--a-read-only-role-with-pg_monitor) — pgbot names the exact GRANT
at connect time.

</details>

<details>
<summary><strong>Deltas are missing on a database I've inspected before</strong></summary>

Statistics were reset or the server restarted; pgbot suppresses deltas rather
than reporting a fake −99% change. On serverless Postgres this is expected —
see [Serverless Postgres](#serverless-postgres-neon-scale-to-zero).

</details>

<details>
<summary><strong><code>npx pgbot</code> returns E404</strong></summary>

The bare npm name `pgbot` is blocked by npm's package-name-similarity rule; the
package is scoped. Use `npx @pgbot/cli`.

</details>

## Privacy

Nothing leaves the machine unless you ask for it: every command except the AI
layer is entirely local. The only commands that make an outbound call are `pgbot
explain` and `pgbot ask`, which send the same PII-free Context to your configured
model — OpenAI or Gemini (and say so, with a confirmation prompt).

That Context is PII-free by construction: `pg_stat_statements` text is normalized
(`$1` placeholders), and the one raw-SQL source (`pg_stat_activity` for blocking
chains) is scrubbed of string/numeric literals, emails, and UUIDs before it can
enter the Context. Connection strings are redacted in every log, error, and
output. This holds for a reader of the source, not just as a claim.

## Contributing

Issues and PRs welcome — see [CONTRIBUTING.md](CONTRIBUTING.md) for the
invariants that are load-bearing (read-only, deterministic findings, PII-free
output) and the dev loop: `go build ./cmd/pgbot`, `go test ./...`
(DB-dependent tests self-skip), and `scripts/gate.sh` before pushing.
`make matrix` runs the suite against the PostgreSQL matrix in
`docker-compose.test.yml`.

## Security

pgbot handles connection strings and reads production statistics. To report a
vulnerability, use GitHub's **private vulnerability reporting** (Security →
Report a vulnerability) — please don't open a public issue. Scope and response
expectations are in [SECURITY.md](SECURITY.md).

## License

[Apache-2.0](LICENSE).
