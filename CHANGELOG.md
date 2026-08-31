# Changelog

All notable changes to pgbot are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/), and the project aims for
[Semantic Versioning](https://semver.org/). The `--json` contract is versioned
separately by `model.SchemaVersion` (currently 1.3.0).

## [Unreleased]

### Added
- **Experimental CockroachDB diagnostics.** Engine-aware collection now covers
  cluster and workload health, live and persisted queries, contention,
  execution insights, indexes, tables, jobs, range distribution, hotspots,
  storage, replication recovery, and Raft queues. Focused terminal screens and
  CockroachDB-grounded `ask` / `explain` summaries use the same deterministic,
  PII-free context. The JSON contract advances additively to **1.3.0**.
- **PgDog poolers are identified behaviorally, never by hostname** (#22). The
  connect probe now sets the `pgdog.shard` routing hint session-level alongside
  a control GUC and reads both back: PgDog consumes `pgdog.*` hints instead of
  forwarding them, so a vanished hint with an intact control can only be PgDog
  — on any hostname and port (verified against PgDog v0.1.54). The control
  keeps a PgBouncer backend switch from ever reading as a false PgDog.

### Fixed
- **Hosts containing `-pooler` are no longer labeled "a Neon pooled
  endpoint" unless they are on Neon's own domain** (#22). PgDog and
  self-hosted poolers reuse the `-pooler` naming convention; those endpoints
  still count as a pooler signal but now carry the generic label.

## [0.5.1] - 2026-08-24

### Fixed
- **`pgbot why` polish from the first real-world runs.** Sub-millisecond means
  no longer render as "0ms → 0ms" on a real slowdown (precision now scales
  with magnitude: 0.04ms → 0.39ms) and tiny per-second rates keep their
  significant digits; when the store holds history older than the window,
  the too-few-snapshots message says how many more exist and names the
  `--window` widening; and the pick-one database listing carries server
  version, provider, and recency, so six databases all named "postgres"
  are tellable apart (snapshots deliberately store no host).
- **The recurring Windows npx-smoke false alarm was npx cache poisoning, not
  slow propagation.** A first attempt that runs before `@pgbot/win32-x64` has
  propagated caches the wrapper WITHOUT its platform optionalDependency, and
  every retry silently reuses that poisoned tree — which is how the
  0.4.2/0.4.3/0.5.0 Windows smokes stayed red for their whole retry budget
  while the registry was verifiably fine. The smoke job now gives each attempt
  a virgin npm cache, and the wrapper's no-binary error tells real users who
  hit the same trap how to retry with a fresh cache.

## [0.5.0] - 2026-08-24

### Added
- **`pgbot why` — deterministic root-cause chains from baseline history.** The
  correlation feature the roadmap promised: per-object time series over the
  stored snapshots (each `inspect` adds one), sustained-shift onset detection,
  and explicit mechanism rules connecting a symptom to its cause — *"query 42
  slowed 3.2× — mean 8ms → 26ms per call · because seq scans on public.orders
  surged 0.1 → 50 per second · after the table grew 18%"* — with the numbers
  and onset times on every hop. v1 ships the query-slowdown rule: interval
  mean (ΔTotalMS/ΔCalls, honest where pg_stat_statements' lifetime mean
  dilutes fresh regressions) mechanized by a seq-scan surge on a referenced
  table, with table-growth and index-dropped antecedents. Temporal discipline
  is a hard gate (a cause whose onset follows the symptom never chains);
  confidence comes from onset alignment, antecedents, and magnitude, and
  anything below 0.5 is worded as a possibility. Fully offline, like `diff`;
  needs ≥3 snapshots and says exactly what to run when it has fewer. `--json`
  emits a separately versioned report (`why_schema_version: 1.0.0`); the MCP
  server gains a matching `why` tool. Counter resets split series rather than
  fabricating rates; missing top-N entries are gaps, never interpolated. The
  output explains itself: what was analyzed ("analyzed N queries and M
  tables"), how many regressions were found vs shown ("showing the 5 worst of
  7"), and a how-to-read legend; `pgbot why 10` (or `--max-chains`) widens the
  default 5, and the JSON carries the same scope fields
  (`analyzed_queries`/`analyzed_tables`/`regressions_found`).
- **pgaudit posture findings.** When the pgaudit extension is installed, pgbot
  grades its configuration (pgbot cannot read the audit trail itself — it lives
  in the server log — but every knob that decides whether the trail exists is
  visible in pg_settings): `pgaudit_silent` (warn) — installed but
  `pgaudit.log` selects no classes, the compliance foot-gun where the audit
  trail everyone relies on does not exist; `pgaudit_logs_parameters` (warn,
  risk) — `pgaudit.log_parameter=on` writes bind parameters (passwords, PII)
  into plaintext server logs; `pgaudit_double_logging` (info) — pgaudit session
  logging alongside `log_statement=all` records every statement twice. Each
  ships with a catalogue page (`pgbot explain-finding pgaudit_silent`),
  suppression support, and cluster-wide dedupe under `--all-databases`.

## [0.4.3] - 2026-08-22

### Fixed
- **Eight audit fixes (#20, #21), contributed by @10xdev4u-alt.** The baseline
  store's 100 MB cap now actually reclaims space (DELETE never shrinks a
  WAL-mode SQLite file; the promised VACUUM never ran, so every run evicted
  another 10% of history); JUnit output no longer fails the test pane on
  `Preexisting` findings the exit code passes under `--fail-on-new`; Prometheus
  label values are escaped exactly once (a database name with a quote or
  backslash was double-escaped, changing the exposition bytes); the MCP
  `diagnose` prompt no longer renders the DSN — password included — into
  prompt text; `archiving_stalled` honors `archive_timeout` (the value is
  unit-suffixed — `5min`, `1h` — so the old `Atoi` parse was a dead branch and
  the threshold stuck at the 1h floor, firing false criticals);
  `checksum_failures` is reported once under `--all-databases` while
  `work_mem_low` correctly stays per-database; `install.sh` pins the cosign
  signing identity to the release workflow instead of accepting any workflow
  in the repo; finding text is truncated by rune, never mid-UTF-8-sequence.
- **Review follow-ups on the above.** Cluster-wide dedupe keeps the first
  occurrence of a finding instead of assuming the first database carries it
  (a permissions failure on database one could erase a live corruption
  report); the store VACUUMs before any eviction (an upgraded, free-page-bloated
  file no longer costs the snapshot just saved), evicts in one sized pass
  instead of up to twenty full-file rewrites, never deletes the last snapshot,
  and treats VACUUM contention as best-effort so a parallel `--all-databases`
  run can't lose its schema/events writes; the `diagnose` prompt drops its
  `connection_string` argument entirely (prompt arguments never reach the
  model — the rendered text was the only carrier, and that was the leak) and
  directs agents to the server's `DATABASE_URL`; the cosign identity regexes in
  `install.sh`, `release.yml`, and the README anchor the workflow filename
  (`release.yml@`) so a similarly-prefixed workflow can't satisfy them; the
  Prometheus exposition gains a `preexisting` label and stops counting
  preexisting findings in `pgbot_findings_total`, matching the exit code; the
  `--full` findings view marks preexisting findings and keeps them out of the
  headline counts; the index advisor's query line truncates by rune.

## [0.4.2] - 2026-08-21

### Added
- **`uses: pgrundev/pgbot@v1` now actually resolves.** The composite GitHub
  Action moved from `.github/actions/pgbot/` to the repository root — the
  location the `owner/repo@tag` syntax (and the Marketplace) requires — and a
  floating `v1` tag tracks it. `release.yml` now triggers only on full
  `vX.Y.Z` tags so the major tag can never cut a release by accident.
- **`pgbot init` — guided setup that never touches the database.** Generates
  the canonical read-only role SQL (`CREATE ROLE … LOGIN`, `GRANT pg_monitor`,
  `GRANT CONNECT`) plus the provider-appropriate `pg_stat_statements` step —
  executable where the extension is preloaded (Supabase, Neon), commented
  instructions where preload comes first (RDS, Aurora, Cloud SQL, Azure,
  self-hosted). With a connection string it detects the database name and
  provider; the output is pipe-safe by contract (every line is a statement, a
  `--` comment, or blank), so `pgbot init | psql "$ADMIN_DSN"` is the intended
  path — pgbot itself executes nothing. `pgbot init --verify` connects as the
  monitoring role and checks the prerequisites (pg_monitor critical,
  pg_stat_statements warn with the provider fix, standby per-node-counter
  note), exiting non-zero when the critical one is missing.

### Security
- **Every third-party GitHub Action is pinned to a commit SHA** — CI, the
  release pipeline, and the published composite action (whose
  `upload-sarif` step runs inside end-users' workflows). Dependabot maintains
  the pins and gains a 14-day cooldown for gomod and github-actions version
  updates (security advisories are not delayed). Contributed by @lpmi-13 (#14).

## [0.4.1] - 2026-08-19

### Fixed
- **`pg_stat_statements` installed outside `public` was detected but unreadable
  (#10).** Supabase (and any `CREATE EXTENSION … SCHEMA x`) puts the extension's
  objects in `extensions`; pgbot's probe saw it in `pg_extension` but every read
  used the bare relation name, so `queries` came back
  `unavailable: relation "pg_stat_statements" does not exist` while the server
  capability list still said `pg_stat_statements` — a silent loss of the report's
  highest-value section for a dedicated read-only role whose `search_path` doesn't
  include the schema. The probe now records the namespace of every installed
  extension (`Capabilities.ExtensionSchemas`) and the fixed, allowlisted object
  names — the `pg_stat_statements` view, the `pg_stat_statements(showtext)` SRF,
  `pg_stat_statements_info`, and hypopg's `hypopg_create_index` /
  `hypopg_relation_size` / `hypopg_reset` used by `advise` — are addressed
  schema-qualified and identifier-quoted (`"extensions"."pg_stat_statements"`),
  independent of `search_path`. When the schema can't be read the bare name is
  used, i.e. the previous behaviour. Covered by an integration test that
  relocates the extension and runs the read-only role against it.
- **`index_invalid` no longer overstates failed-build debris as critical write
  overhead (#11).** A `CREATE INDEX CONCURRENTLY` that fails during the build
  (a duplicate key on a unique build, a timeout, a cancelled session) leaves
  `indisvalid = false, indisready = false` and a 0-byte relation — an index
  PostgreSQL **ignores on INSERT/UPDATE**. pgbot graded every invalid index
  `critical` (impact 85) with the blanket claim "still maintained on every write",
  ranking that debris above live operational problems. The schema fingerprint
  now carries `indisready`, `indislive`, and `pg_relation_size` for invalid
  indexes, and each one is classified: `indisready = true` → maintained on every
  write, never read → **critical** (unchanged); `indisready = false` →
  failed-build debris, not maintained on writes → **warn** (impact 45) with
  cleanup guidance ("the index you meant to have does not exist"), never a
  write-cost claim; `indislive = false` → being dropped → warn. Evidence lines
  carry the state and size (`… indisready = false: failed-build debris, NOT
  maintained on writes (0 B)`), the impact estimate says how many are actually
  maintained, and the finding page's verify query shows both flags. The
  in-progress-build downgrade (warn, confidence 0.5, do-not-drop guard) is
  unchanged. Not a JSON contract change: the classification rides on the
  existing `severity` / `evidence` / `impact` fields.
- **`brew install pgrundev/tap/pgbot` works (#8).** The README advertised the tap
  since 0.3.0, but the `pgrundev/homebrew-tap` repository was never created and
  the release's formula push was gated on a `HOMEBREW_TAP_TOKEN` that was never
  set — so every release stayed green while the documented install failed with
  "Repository not found". The tap now exists with a formula for the current
  release (macOS Intel/Apple Silicon, Linux x86_64/arm64, SHA-256 pinned to the
  signed release archives), and GoReleaser pushes the regenerated formula on
  every tag over git+SSH with a **deploy key scoped to the tap repo**
  (`HOMEBREW_TAP_DEPLOY_KEY`) instead of a personal access token. A new
  post-release `brew-smoke` job `brew install`s the tag on a fresh macOS runner
  and **fails the release run** if the formula wasn't published, so this can't
  silently regress again. Release procedure documented in `docs/release.md`.
- **`npx pgbot` → `E404` is documented, not a bug to chase (#9).** npm's
  name-similarity policy blocks creating the bare `pgbot` package (too close to
  `got`); the wrapper has been `@pgbot/cli` since 0.3.3. The README now says so
  explicitly next to the npx row, and the 0.3.0 release notes that advertised
  `npx pgbot` carry a correction.

### Changed
- Updated pgx to v5.10.0; GitHub Actions bumped (`actions/setup-go` v7,
  `docker/login-action` and `docker/setup-buildx-action` v4). No behaviour
  change; `govulncheck` still reports no vulnerabilities.

## [0.4.0] - 2026-08-19

### Added
- **Index/code correlation (`pgbot indexes --correlate`, MCP `index_code_correlation`).**
  pgbot grades every unused / redundant / invalid index by how the drop can be
  *proven*, and hands an agent exactly what to search for — without ever reading
  your repository:
  - `catalog_proven` — invalid or redundant/duplicate; provable from the catalog
    alone, no code check, no stats-window caveat.
  - `needs_code_check` — a zero-scan plain btree over bare columns. pgbot emits the
    identifiers to grep in every case convention (camelCase, snake_case,
    PascalCase, CONSTANT_CASE) plus the load-bearing instruction: search *filter*
    positions only (WHERE / JOIN / ORDER BY / GROUP BY / ORM filters), never SELECT
    lists — and how to read a hit vs. a miss.
  - `inconclusive` — GIN/GiST/BRIN, expression, partial, or a cold window. These
    can serve a query shape that simply hasn't run, so they keep "do not DROP INDEX
    on this evidence" and are **never** promoted to actionable by an empty code
    search. pgbot never reads the repo and never drops anything.
- **Verdict write-back (MCP `record_index_verdict`).** An agent records what its
  repo search found (`found_in_code` / `not_found_in_code` / `inconclusive`),
  stored locally per database. On a later run the same still-unused index carries
  the prior verdict forward and notes when the zero-scan window has since grown —
  a one-off grep becomes compounding evidence. New `index_verdicts` store table
  only; no existing table changes.
- **Replica-identity indexes are never reported as unused.** A `REPLICA IDENTITY
  USING INDEX` index shows zero scans on the primary but dropping it breaks logical
  replication and UPDATE/DELETE row identity — now excluded alongside PK / unique /
  exclusion / FK-backing indexes.
- **Destructive-action guards are now structured and guaranteed (`finding.safety`).**
  Every finding whose remediation involves a destructive or irreversible action
  (DROP INDEX, VACUUM FULL, REINDEX, DROP REPLICATION SLOT, a table rewrite) now
  carries machine-actionable guards — `{id, kind: prohibition|precondition, action,
  text, verify}` — instead of leaving the warning to free-form prose a summarizing
  model could drop. They are emitted deterministically in code and guaranteed in
  `--json`, SARIF, the MCP payloads, and both terminal views. Two guards that
  previously existed **only** in docs pages are now on the finding itself: the
  wraparound "don't VACUUM FULL / don't consume XIDs" guard, and the "don't drop a
  replication slot a live standby still depends on" guard (whose remediation no
  longer nudges toward the drop before the check). `pgbot ask` / `explain` reassert
  these guards from code, after the model's text, so the model cannot omit them. A
  build-failing regression test fails CI if a destructive remediation ships without
  a guard. The guards are also carried by SARIF, JUnit (`<failure>` text), and a
  Prometheus `destructive="true"` label; a test AST-scans the render package and
  fails the build if a new output surface ships without carrying them. **For a
  database with a destructive finding, the default and `--full` terminal views now
  add a `⚠` guard line — clean databases are byte-identical.**
- **Verdict strengthening is bounded (index/code correlation).** A stored code
  search plus a growing window is the one place confidence could rise on its own,
  so: a verdict older than the current stats window is marked **stale** (`stale`,
  `age_days`), its age is stated in output ("code check is 47 days old — the
  repository may have changed since"), and a stale verdict never strengthens. The
  strengthened wording reads as corroboration, never authorization (the phrases
  "safe to drop" / "confirmed unused" are never generated), and the `precondition`
  guard persists through any verdict. An `inconclusive` index is never promoted by
  any verdict at any window length. The `if_not_found` caveat now always names
  monthly/quarterly/annual jobs a long window still can't see.

### Changed
- `model.IndexStat` gains `columns`, `method`, `unique`, and `primary` (additive).
  JSON contract `SchemaVersion` → **1.2.0**; a 1.1.0 consumer still parses 1.2.0
  output unchanged.

## [0.3.3] - 2026-08-18

### Changed
- **The npm wrapper is published as `@pgbot/cli`, not `pgbot`.** npm's package-name
  similarity policy blocks the bare name `pgbot` from being created (too close to
  the existing `got`/`hubot` packages), which failed 0.3.2's publish after the six
  platform packages had already gone up. The wrapper now uses the scoped name we
  own: install with `npx @pgbot/cli inspect "$DATABASE_URL"` or
  `npm i -g @pgbot/cli`. Nothing else changes — the installed command is still
  `pgbot`, the six `@pgbot/<os>-<arch>` binary packages are unchanged, and the
  Homebrew formula, `install.sh`, Docker image, and `go install` path are
  unaffected.

## [0.3.2] - 2026-08-18

### Fixed
- Re-cut of 0.3.1 to publish the npm packages — 0.3.1's npm step failed because a
  CI publish needs a 2FA-bypass/automation token. No code changes versus 0.3.1
  (the binaries, Docker image, and signatures are identical). npm is now live:
  `npx @pgbot/cli inspect "$DATABASE_URL"`.

## [0.3.1] - 2026-08-18

### Fixed
- **pgbot no longer measures its own footprint as the database's** (from external
  PR #1 by @mishafyi, measured against a real remote PG18). Several places where
  the read path counted, timed, or reported pgbot's own sessions and session pins:
  - Settings reported pgbot's own session pins (`statement_timeout=15s`, etc.) as
    the server's non-default parameters; now reads the server's real values via a
    transaction-local unpin.
  - Connection count now counts only client backends (not autovacuum/checkpointer/
    walwriter/IO workers), and never pgbot's own pool.
  - The Aurora probe called `aurora_version()`, which errored and booked a rollback
    on every non-Aurora server each run; now detected from `pg_proc`.
  - pg_stat_statements reads no longer spill to temp files (transaction-local
    `work_mem`), so pgbot doesn't report its own `temp_bytes`.
  - The wait sampler's per-poll deadline was too short for a remote link (every
    poll timed out); a fixed budget makes the wait profile work over the internet.
  - `low_cache_hit` requires enough block traffic before grading (a thin sample was
    flipping the finding and the exit code on noise); `vacuum` grades "due?" against
    the actual autovacuum knobs and per-table reloptions; the real index count is
    reported (not the LIMIT-200 scan); idle `Client` waits aren't counted as
    "waiting"; and TPS excludes pgbot's own transactions.

### Added
- **npm distribution is live**: `npx @pgbot/cli inspect "$DATABASE_URL"`.
- Release self-checks: the published image must be anonymously pullable and the
  cosign signature must verify, both asserted after every release.

## [0.3.0] - 2026-08-17

### Added
- **Schema profile for CI (`--profile=schema`, `pgbot lint`).** Runs only the
  findings derivable from the catalog alone — invalid/redundant indexes, unindexed
  foreign keys, a narrow identity column, autovacuum disabled on a table — so it's
  safe against an empty, freshly-migrated database, where the full profile would
  fire `unused_indexes` and `stale_statistics` on everything. A schema report says
  so in its header and makes no claim about a running database's health.
- **`--fail-on-new <base.json>`.** Compare a run against a base report and act only
  on findings the change introduced — new findings, escalated severities, and new
  rows inside an existing aggregate (a fourth unindexed FK on top of three).
  Pre-existing findings are marked `preexisting: true` in `--json`, excluded from
  SARIF and the exit code. This is the migration-PR check: schema profile + base
  vs. head, only regressions fail. The GitHub Action gains `profile` and
  `base-report` inputs.
- **New finding `int4_identity_column`.** A sequence-backed `int4`/`serial` (or
  identity) column wraps at 2.1 billion — `int2` at 32767 — regardless of its
  current value, after which the next insert errors. Detected structurally, so it
  fires on the migration PR while the fix is still free, where the value-based
  `sequence_exhaustion` cannot. **Note:** this is a new finding ID, so anyone with
  a `.pgbot.toml` will see it for the first time and it will fire on serial primary
  keys immediately, some deliberately — scope an `[[ignore]]` to the bounded tables
  you've reasoned about. Its severity is not yet weighted by production table size
  (planned), so read it as "will wrap eventually", not "wraps soon".
- **npm distribution**: `npx @pgbot/cli inspect "$DATABASE_URL"` runs with no prior
  install. The prebuilt binary ships as a per-platform `optionalDependency`
  (`@pgbot/<os>-<arch>`), so it lands in the lockfile with an integrity hash,
  needs no network beyond the registry, and works with `npm ci --ignore-scripts`
  — no `postinstall` download. The wrapper passes argv, stdio, signals, and the
  exit code through verbatim, published from the release tag with npm provenance.

### Changed
- Releases now sign the checksums into a self-contained **cosign bundle**
  (`checksums.txt.cosign.bundle`), and `install.sh` verifies it with
  `cosign verify-blob --bundle` — no longer relying on the `--certificate` /
  `--signature` flags cosign v3 has deprecated. The detached `.sig`/`.pem` are kept
  this release as a fallback.

### Fixed
- The GitHub Action's default `version: latest` no longer 404s. `install.sh`
  treated `latest` as a literal release tag (`pgbot_latest_..._.tar.gz`, a 404);
  it now resolves `latest` via the releases API like an empty value, and the
  Action passes an empty version rather than the literal string. The Action also
  installs into the same `~/.local/bin` it adds to `PATH` instead of disagreeing
  with the installer's default.

## [0.2.1] - 2026-08-17

### Fixed
- **pgbot no longer counts its own connections as findings.** pgbot samples
  through a small connection pool; between short READ ONLY samples each
  connection is briefly idle in a transaction and holds an xmin. The
  pg_stat_activity queries excluded only the single querying backend, so sibling
  pool connections were intermittently counted — a flaky false positive on an
  otherwise-quiet database (`N session(s) idle in transaction` with nothing
  actually idle, a self-pinned vacuum horizon, connection-saturation slots pgbot
  was itself consuming, wait-profile noise, and pgbot listed in its own
  connection breakdown). Every pg_stat_activity query now excludes all of
  pgbot's own backend PIDs — captured when the pool warms, so the exclusion is
  unspoofable (a session can't hide by naming itself `pgbot`) and never affects a
  user service that happens to be named `pgbot`.
- Installer: `PGBOT_INSTALL_DIR` is created if it doesn't exist (a custom path
  like `~/.local/bin`), instead of falling through to an unexpected `sudo`
  prompt.

### Changed
- Installer signature verification prefers a self-contained cosign bundle
  (`checksums.txt.cosign.bundle`) when present, so it no longer depends on the
  `--certificate` / `--signature` flags cosign v3 has deprecated; it falls back
  to the detached certificate + signature when no bundle is published.

## [0.2.0] - 2026-08-17

### Added
- **Index advisor** (`pgbot advise`): missing-index suggestions, each validated
  by the planner with hypopg — nothing is built. Also the MCP `suggest_indexes`
  tool. Requires hypopg + pg_stat_statements + PostgreSQL 16+.
- **Configuration & suppression** (`.pgbot.toml`): per-object `[[ignore]]` rules
  (with expiry and dead-rule detection), `[severity]` remaps, `[thresholds]`
  overrides, and `pgbot config check` / `explain` / `init`. Suppression is always
  visible and never hides a critical or affects the exit code silently.
- **Findings catalogue**: a `docs/findings/<id>.md` page for every finding, an
  offline `pgbot explain-finding <id>`, and a by-dimension index.
- **`pgbot diff`**: compare two baseline snapshots offline, honest about the
  interval it actually used and about resets/evictions between them.
- **`pgbot inspect --all-databases`**: sweep every non-template database in the
  cluster; cluster-wide findings are reported once, not once per database.
- **Recoverability findings**: WAL archiving health, data-checksum failures,
  synchronous-replication degradation, replica lag, stale statistics, and
  autovacuum health.
- **CI-pipeline output**: `--fail-on=<severity>`, `--format=sarif` (uploads to the
  GitHub Security tab), `--format=junit`, `--format=prometheus` (node_exporter
  textfile), and a `pgrundev/pgbot` GitHub Action.
- **JSON Schema** for the `--json` contracts, published as release assets.
- **Windows** builds (amd64, arm64) and per-artifact CycloneDX SBOMs.

### Changed
- **Baseline fingerprints are now per-database within a cluster.** Previously a
  baseline was keyed on the cluster-wide `system_identifier` alone, so snapshots
  from different databases on the same server were merged into one series and
  their deltas were meaningless. The key now includes the database name.
  **On upgrade:** snapshots written by v0.1.x used the old cluster-wide key and
  will not match new per-database runs — those series effectively reset. Old
  snapshots are left in place (the `system_identifier` isn't stored in a snapshot,
  so they can't be recomputed); pgbot prints a one-time notice on the first run,
  and you can clear the stale series with `pgbot baselines prune <fingerprint>`.
- Exit codes are precise and documented: `0` clean · `1` warn · `2` critical ·
  `3` connection/execution failure · `64` usage error. Suppressed findings never
  contribute.

### Security
- **Fixed an information-disclosure defect in `pg_stat_statements` handling.**
  pg_stat_statements normalizes ordinary queries but stores *utility* statements
  (e.g. `CREATE USER … PASSWORD`, `ALTER ROLE`, `DO` blocks, `COPY … FROM PROGRAM`)
  verbatim. The `queries` collector trusted that text as already-parameterized and
  did not scrub it, so a literal secret in such a statement could appear in a
  `--json` report and, through `pgbot explain` / `ask`, be sent to an external
  model. All pg_stat_statements text is now scrubbed before it leaves the process.
  **If you ran a v0.1.x `queries`/`--json`/`explain`/`ask` and shared the output,
  treat any credential in a recent utility statement as exposed and rotate it.**
- **Fixed a dropped redaction marker in query-text scrubbing.** Dollar-quoted
  spans were replaced using regex Expand semantics, so the `$REDACTED$` marker
  parsed as an empty capture-group reference: the sensitive span was removed but
  came out blank instead of marked. Scrubbing now uses literal replacement and is
  covered by a fuzz test.
- Updated pgx to v5.9.2 (fixes a SQL-injection advisory), the Go toolchain to
  1.25.13, and golang.org/x/text to v0.39.0; `govulncheck` now runs in CI and
  reports no vulnerabilities.

[0.5.1]: https://github.com/pgrundev/pgbot/releases/tag/v0.5.1
[0.5.0]: https://github.com/pgrundev/pgbot/releases/tag/v0.5.0
[0.4.3]: https://github.com/pgrundev/pgbot/releases/tag/v0.4.3
[0.4.2]: https://github.com/pgrundev/pgbot/releases/tag/v0.4.2
[0.4.1]: https://github.com/pgrundev/pgbot/releases/tag/v0.4.1
[0.4.0]: https://github.com/pgrundev/pgbot/releases/tag/v0.4.0
[0.3.3]: https://github.com/pgrundev/pgbot/releases/tag/v0.3.3
[0.3.2]: https://github.com/pgrundev/pgbot/releases/tag/v0.3.2
[0.3.1]: https://github.com/pgrundev/pgbot/releases/tag/v0.3.1
[0.3.0]: https://github.com/pgrundev/pgbot/releases/tag/v0.3.0
[0.2.1]: https://github.com/pgrundev/pgbot/releases/tag/v0.2.1
[0.2.0]: https://github.com/pgrundev/pgbot/releases/tag/v0.2.0
