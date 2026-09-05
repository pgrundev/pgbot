---
name: postgres-diagnostics
description: Diagnose PostgreSQL health and performance using the read-only pgbot tool. Use for slow databases, expensive queries, unused indexes, disk usage, bloat, autovacuum, vacuum health, and configuration questions.
---

# PostgreSQL diagnostics with pgbot

pgbot is a **read-only** Postgres diagnostic tool. Its findings are computed
**deterministically** (in Go, not by a model) and it **never writes** to the
database. Treat pgbot's output as the source of truth. Your job is to run the
right command, then interpret, prioritize, and recommend — never to invent
diagnoses or act on the database.

## When to use this skill

- "why is my database slow", "what should I optimize", "is my DB healthy?"
- "which indexes can I drop", "what's using all the disk", "is autovacuum keeping up?"
- "what query is eating the database", "should I re-tune anything?"
- Any Postgres performance / health / bloat / index / vacuum / config question.

## How to run it

**If the pgbot MCP server is connected**, call its tools: `inspect` (start here),
`top_queries`, `vacuum_health`, `unused_indexes`, `why`. They return stable JSON.
(The `logs`, `waits`, `activity`, `erd`, and `report` commands are CLI-only for
now — run them through the shell.)

**Otherwise run the CLI.** It needs a connection string for a read-capable role
(ideally one with `pg_monitor`). Pick the command that matches the question:

| Question | Command |
|---|---|
| Open-ended / "is it healthy" | `pgbot inspect "$DSN"` (health score + worst-first findings) |
| Full detail | `pgbot inspect "$DSN" --full` |
| "What's eating the database?" | `pgbot queries "$DSN"` (top by total time; `--by-calls` to re-rank) |
| "What's using disk / biggest tables?" | `pgbot tables "$DSN"` (size + seq-vs-index scan = missing-index radar) |
| "Is autovacuum keeping up?" | `pgbot vacuum "$DSN"` |
| "Which indexes can I drop?" | `pgbot indexes "$DSN"` |
| "Should I re-tune config?" | `pgbot tune "$DSN"` |
| "Why is it slow RIGHT NOW?" | `pgbot why --duration 10s "$DSN"` (live wait sampling → evidence-gated cause: lock contention vs IO vs CPU vs client; refuses to guess on thin evidence) |
| "Who is connected / what's running?" | `pgbot activity "$DSN"` (live sessions: PIDs, states, waits, ages, scrubbed SQL) |
| "Where does database time go?" | `pgbot waits --duration 10s "$DSN"` (sampled wait classes + blockers named only with sustained evidence) |
| "What changed since yesterday?" | `pgbot why "$DSN"` / `pgbot diff --since 24h` (offline, from stored snapshots) |
| "What's in the server log?" | `pgbot logs "$DSN"` (`--live` to follow, `--level error` or `--level audit` to filter; needs one extra grant, printed when missing) |
| "What does the schema look like?" | `pgbot erd "$DSN"` (`--mermaid` for a renderable diagram, `--html > schema.html` for an interactive file) |
| Full report for a human to read | `pgbot report "$DSN" > report.html` (self-contained page: findings, queries, indexes, waits) |
| Machine-readable for parsing | `pgbot inspect "$DSN" --json` |

Add `--timeout 60s` for large or remote databases.

## First run — when a prerequisite is missing

Don't stall and don't improvise; each gap has one fix:

- **`pgbot` not on PATH:** install it — `curl -fsSL https://pgbot.dev/install | sh`
  (or `brew install pgrundev/tap/pgbot`, or run without installing via
  `npx @pgbot/cli`). Run it through the normal permission prompt so the user
  sees the command.
- **No connection string:** check `$DATABASE_URL`, then ask the user for one.
  Never guess or assemble credentials yourself.
- **Role or extension gaps:** if the installed pgbot has `init`, run
  `pgbot init --verify "$DSN"` — it checks pg_monitor and pg_stat_statements
  and names each fix. Older builds report the same gaps at connect time.
  To create the read-only role, generate the SQL with `pgbot init` (or use the
  Setup section of the pgbot README) and **hand it to the user to run as
  admin** — never execute it yourself: pgbot never writes, and neither do you.

## Rules — non-negotiable

1. **Findings are facts.** Do not invent a number, table, index, or query id
   beyond what pgbot reports. If pgbot didn't measure it, say what pgbot would
   need to collect to find out.
2. **Carry every caveat into the recommendation.** Example: "unused index" scan
   counts are per-node — on a primary, a replica may still use an index that
   looks unused. pgbot flags this; **never recommend dropping an index without
   its caveat.** (pgbot's `tables`/`indexes` and the JSON make replication state
   visible — check it.)
3. **Never execute the user's query to diagnose it.** No `EXPLAIN ANALYZE`, no
   running the statement "just to time it." Suggest only safe, non-executing
   steps (`EXPLAIN` without `ANALYZE` is fine; it doesn't run the query).
4. **Prioritize by impact, not by count.** Risk (time-to-incident: wraparound, a
   WAL-pinning replication slot, a filling disk) comes first. Then the biggest
   latency/storage win. A 9 GiB unused-index reclaim may matter *less* than one
   query eating 60% of DB time — say which to do first and why.
5. **Never write to the database.** pgbot is read-only; you recommend, you don't
   act. Hand the user the exact `DROP INDEX CONCURRENTLY` / `ALTER` / `VACUUM`
   statement to run themselves.
6. **Hedge low confidence.** A finding below 0.5 confidence is a possibility
   ("may", "possibly"), not an assertion.
7. **Label the evidence.** Keep three categories visibly distinct in your
   answer: findings proven by pgbot's data, recommendations that still need
   review (of code, replicas, or workload), and conclusions blocked by missing
   statistics or permissions.

## Output shape

1. One-line health verdict.
2. Worst-first, at most ~3 issues. For each: the problem *with pgbot's number* →
   a likely cause **only if the data (deltas/events) supports one** → a safe,
   concrete recommended step, with its caveat inline.
3. Briefly name what's healthy, so the user knows what was checked.

## Reading the signals

- **`tables`:** a large table with heavy `seq scans` and few `idx scans` is a
  likely missing-index candidate — cross-check it against `queries` (a top query
  filtering that table confirms it).
- **`queries`:** `share` is % of total DB execution time. A single query above
  ~30% is the real hot path; index-dropping won't touch it.
- **`vacuum`:** `due? yes` with a stale/`never` last-autovacuum means autovacuum
  is falling behind — the early signal for bloat and, downstream, wraparound.
- **replication-slot findings:** an inactive slot retaining WAL fills the disk;
  treat it as time-to-incident, not cosmetic.

## Safety and privacy

pgbot only ever reads. Nothing leaves the machine except `pgbot explain` /
`pgbot ask`, which send the **PII-free** findings (normalized query text, no
literals) to an LLM and say so first. Use a role with `pg_monitor`; never paste a
production superuser credential where it can leak.
