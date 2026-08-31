---
id: crdb_job_failed
severity: warn
critical_when: job state is revert-failed
dimension: risk
object: job
scope: infra
requires: [information_schema.crdb_jobs_with_progress]
thresholds: []
related: [crdb_job_stalled, crdb_job_reverting]
---

# crdb_job_failed

**Severity:** warn · **Dimension:** risk · **Object identity:** `job`

## What pgbot observed

A job failed within the last 24 hours. A `revert-failed` job is critical because cleanup of the original operation also failed.

## Why it matters

Schema changes, backups, restores, imports, and maintenance may be incomplete.

## How to verify it yourself

```sql
SELECT job_id, job_type, state, error
FROM information_schema.crdb_jobs_with_progress
WHERE state IN ('failed', 'revert-failed');
```

## How to fix it

Inspect the full job record and error, correct the cause, then retry or resume only when that operation supports it. Do not assume that all job types are safely retryable.

## When to ignore it

An intentionally abandoned job may be ignored after confirming it has no unfinished operational impact.

```toml
[[ignore]]
finding = "crdb_job_failed"
object  = "job:<job-id>"
reason = "known abandoned test job"
expires = "2027-01-01"
```

## What pgbot cannot see

pgbot stores only server-redacted and client-scrubbed operation, status, and error summaries. Use `SHOW JOB <id>` for the complete record. It cannot determine whether retrying a particular external operation is safe.

## Related

- [crdb_job_stalled](crdb_job_stalled.md)
- [crdb_job_reverting](crdb_job_reverting.md)
