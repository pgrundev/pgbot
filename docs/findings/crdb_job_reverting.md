---
id: crdb_job_reverting
severity: info
critical_when: ""
dimension: risk
object: job
scope: infra
requires: [information_schema.crdb_jobs_with_progress]
thresholds: []
related: [crdb_job_failed, crdb_job_stalled]
---

# crdb_job_reverting

**Severity:** info · **Dimension:** risk · **Object identity:** `job`

## What pgbot observed

A job is actively reverting an operation that did not complete. A revert that has not updated for 30 minutes is reported as `crdb_job_stalled` instead.

## Why it matters

CockroachDB is cleaning up the failed operation. The original schema change, backup, restore, import, or maintenance task did not achieve its intended result.

## How to verify it yourself

```sql
SELECT job_id, job_type, state, progress_fraction,
       status_message, last_updated, error
FROM information_schema.crdb_jobs_with_progress
WHERE state = 'reverting';
```

## How to fix it

Let an advancing revert finish while monitoring it. After cleanup completes, inspect the original error and correct the cause before retrying the operation.

## When to ignore it

A recent, steadily advancing revert needs monitoring rather than immediate intervention and may be ignored during a controlled recovery window.

```toml
[[ignore]]
finding = "crdb_job_reverting"
object  = "job:<job-id>"
reason = "revert is advancing during a monitored maintenance window"
expires = "2027-01-01"
```

## What pgbot cannot see

pgbot cannot predict the remaining revert duration or decide whether the original operation is safe to retry.

## Related

- [crdb_job_failed](crdb_job_failed.md)
- [crdb_job_stalled](crdb_job_stalled.md)
