# Provider compatibility matrix

How pgbot behaves on each managed-Postgres platform: what it can see, what it
can't, and the exact steps to unblock the degraded paths.

> **Status.** The capability details below are **documented** (📄) — from pgbot's
> provider detection/remediation logic and each provider's published behaviour —
> and the connection guidance is in sync with the README. They are **not the same
> as live-verified** (✅): the ⏳ cells still need a real connection to confirm.
> pgbot itself is a client, and the "local/remote client → managed Postgres over
> the wire" model **is** verified live (against a pgrun-hosted PG 18.4 prod DB),
> which is the same shape as every row here. An honest matrix that marks
> documented-vs-verified beats six unearned green checks.

## Summary

| Provider | `pg_monitor` grantable | `pg_stat_statements` | Default DSN pooled | Stats survive idle | Status |
|---|---|---|---|---|---|
| Amazon RDS | 📄 yes, via `rds_superuser` | 📄 preload + reboot | 📄 no (direct) | 📄 yes | 📄 documented |
| Amazon Aurora | 📄 yes, via `rds_superuser` | 📄 preload + reboot | 📄 no (direct) | 📄 yes | ⏳ WAL/IO differ |
| Google Cloud SQL | 📄 yes, via `cloudsqlsuperuser` | 📄 instance flag | 📄 no (direct) | 📄 yes | ⏳ unverified |
| Azure Flexible Server | 📄 yes, via `azure_pg_admin` | 📄 two server params + restart | 📄 no (direct) | 📄 yes | ⏳ unverified |
| Supabase | ⏳ limited (no superuser) | 📄 preloaded | 📄 **yes** (`:6543`) | 📄 yes | 📄 documented |
| Neon | ⏳ limited (no superuser) | 📄 preloaded | 📄 **yes** (`-pooler` host) | ❌ **no** (scale-to-zero) | 📄 documented |

Legend: ✅ confirmed live · 📄 documented, not yet live-tested · ⏳ unknown, needs a live check · ❌ not supported / notable limitation.

pgbot detects the provider from the host, `version()` text, and provider-specific
`pg_settings` markers (`rds.*`, `cloudsql.*`, `azure.*`, `aurora_version()`), so
detection works even when the host is a bare IP or sits behind a proxy.

---

## Amazon RDS / Aurora

- **Connecting:** you can't install on the RDS/Aurora instance (managed, no OS access) — run pgbot from a client that can reach it. For a **private** instance (typical prod) there are two ways in: run pgbot from a small **EC2 in the same VPC** — it reaches the private endpoint over AWS's internal network, so the DB never needs public access, no SSH tunnel, no IP allow-listing, and the only rule is the RDS security group allowing `5432` from the EC2's security group — or keep pgbot on your laptop and reach the endpoint through a bastion with `--ssh-tunnel` (see [Reaching a private database](../README.md#reaching-a-private-database)), which still validates `sslmode=verify-full` against the real endpoint name. For a **publicly accessible** instance, allow your IP in the security group and connect from your laptop.
  ```bash
  pgbot inspect "postgres://pgbot_ro@mydb.abc123.us-east-1.rds.amazonaws.com:5432/appdb?sslmode=require"
  ```
- **Host metrics:** CPU / memory / disk IOPS live in **CloudWatch**, not Postgres — out of reach over a connection string. Everything pgbot computes over SQL works.
- **`pg_monitor`:** grant as the master user (`rds_superuser`): `GRANT pg_monitor TO <role>;`. RDS does not expose OS-superuser, but `pg_monitor` is fully grantable, which is all pgbot needs.
- **`pg_stat_statements`:** add `pg_stat_statements` to `shared_preload_libraries` in the **DB parameter group**, **reboot** the instance, then `CREATE EXTENSION pg_stat_statements;`. This is exactly the string pgbot prints on the degraded path.
- **Pooler:** the default endpoint is a direct connection. RDS Proxy is opt-in; when used it is a transaction pooler and pgbot will note it (rates stay correct).
- **Idle/stats:** always-on instance; cumulative stats persist normally.
- **Capability-gated:** `pg_stat_io` requires PG 16+; `pg_stat_wal` requires PG 14+. Aurora reports storage differently from community Postgres — WAL/IO sections may read differently and need live confirmation.

## Google Cloud SQL

- **`pg_monitor`:** grant via the `cloudsqlsuperuser` role: `GRANT pg_monitor TO <role>;`.
- **`pg_stat_statements`:** set the `cloudsql.enable_pg_stat_statements` instance flag, then `CREATE EXTENSION pg_stat_statements;`.
- **Pooler:** default endpoint is direct; the Cloud SQL Auth Proxy is a TCP proxy, not a statement pooler.
- **Idle/stats:** always-on; stats persist.

## Azure Database for PostgreSQL — Flexible Server

- **`pg_monitor`:** grant as a member of `azure_pg_admin`.
- **`pg_stat_statements`:** add `pg_stat_statements` to **both** the `azure.extensions` and `shared_preload_libraries` server parameters (a restart applies the latter), then `CREATE EXTENSION pg_stat_statements;`.
- **Pooler:** built-in PgBouncer is opt-in on a separate port; the default endpoint is direct.
- **Idle/stats:** always-on; stats persist.

## Supabase

- **Connecting:** prefer the **direct** endpoint (session-scoped) for pgbot; the default pooled endpoint also works (pgbot notes it).
  ```bash
  # direct (session):
  pgbot inspect "postgres://postgres:pass@db.<ref>.supabase.co:5432/postgres?sslmode=require"
  # pooled (Supavisor, transaction mode, :6543):
  pgbot inspect "postgres://postgres.<ref>:pass@aws-0-<region>.pooler.supabase.com:6543/postgres?sslmode=require"
  ```
- **`pg_monitor`:** Supabase does not grant superuser; `pg_monitor` availability to a custom role needs live confirmation. The default `postgres` role has broad but not unlimited access.
- **`pg_stat_statements`:** preloaded — just `CREATE EXTENSION pg_stat_statements;`.
- **Pooler:** the **default pooled connection string uses port `:6543`** (Supavisor / PgBouncer transaction mode). pgbot detects this endpoint and proceeds with a note; rates stay correct because each counter is sampled in its own transaction against cluster-wide shared memory. Use the direct `:5432` endpoint (`--strict-pooler` will insist on it) if you want session-scoped certainty.
- **Idle/stats:** paid tiers are always-on; free tier pauses after inactivity — treat the first run after a pause like a cold window (pgbot's T2 handling applies).

## Neon

- **Connecting:** the pooled endpoint has a `-pooler` host suffix; use the direct host for session-scoped certainty. Neon's default string ships `channel_binding=require` — pgbot **ignores it automatically** (the driver can't do SCRAM channel binding; TLS from `sslmode` still applies) instead of erroring.
  ```bash
  pgbot inspect "postgres://user:pass@ep-xxx.region.aws.neon.tech/dbname?sslmode=require"
  ```
- **`pg_monitor`:** no superuser; grantability to a custom role needs live confirmation.
- **`pg_stat_statements`:** preloaded — just `CREATE EXTENSION pg_stat_statements;`.
- **Pooler:** the pooled endpoint uses a **`-pooler` host suffix** (PgBouncer transaction mode). pgbot detects it and proceeds with a note.
- **Idle/stats:** **scale-to-zero.** After the compute suspends, cumulative statistics are discarded; the first run after a wake sees a near-zero stats window. This is the canonical case pgbot's cold-window detection (T2) exists for — counter-based findings are suppressed until the window exceeds 15 minutes, and the wait-event profile (ASH) may be the only usable signal.

---

## Verification checklist

For each provider, connect a real instance and confirm — promoting the 📄
(documented) and ⏳ (unknown) marks above to ✅ or ❌, and recording the exact
commands that worked:

- [ ] `pg_monitor` can be granted to a non-admin role, and by which admin role
- [ ] `pg_stat_statements` enable steps above are exact and sufficient
- [ ] whether the **default** connection string routes through a pooler (and which port/host)
- [ ] whether pgbot's pooler detection fires on that default endpoint
- [ ] stats retention across an idle/suspend cycle (scale-to-zero behaviour)
- [ ] which capability-gated sections (`pg_stat_io` PG16+, `pg_stat_wal` PG14+, replication) are unavailable and why
- [ ] paste the working, copy-pasteable setup commands

Run `pgbot inspect "<dsn>" --json | jq .server` to capture the detected provider,
version, and capabilities for the record.
