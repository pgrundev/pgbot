---
id: crdb_job_stalled
severity: warn
critical_when: ""
dimension: risk
object: job
scope: infra
requires: [information_schema.crdb_jobs_with_progress]
thresholds: [no progress or status update for 30 minutes]
related: [crdb_job_failed, crdb_job_reverting, crdb_resource_pressure]
---

# crdb_job_stalled

**Severity:** warn · **Dimension:** risk · **Object identity:** `job`

## What pgbot observed

An active job has not updated its progress or status timestamp for at least 30 minutes. When no progress record exists, pgbot measures from job creation. Perpetual CockroachDB system jobs and schema-change GC jobs are excluded from this generic heuristic because long silent periods are normal for them.

## Why it matters

A silent running, pending, reverting, pause-requested, or cancel-requested job may be waiting on admission, an unavailable node or external service, a conflicting operation, or a coordinator that cannot advance.

## How to verify it yourself

```sql
SELECT job_id, job_type, state, progress_fraction, status_message,
       last_updated, error
FROM information_schema.crdb_jobs_with_progress
WHERE state IN ('running', 'pending', 'reverting',
                'pause-requested', 'cancel-requested')
ORDER BY last_updated NULLS FIRST;
```

## How to fix it

Inspect `SHOW JOB <id>`, DB Console job details, node liveness, admission pressure, and any external sink or storage service used by the operation. Do not cancel or retry the job until its current state and type-specific recovery behavior are understood.

## When to ignore it

Some large jobs report progress in coarse steps. Ignore the warning only after confirming independently that the job is still advancing normally.

```toml
[[ignore]]
finding = "crdb_job_stalled"
object  = "job:<job-id>"
reason = "verified coarse progress reporting for this operation"
expires = "2027-01-01"
```

## What pgbot cannot see

The timestamp proves only that the public progress/status record has not changed. It does not prove that every internal worker is idle, and pgbot deliberately does not cancel jobs. Expected-silent system and GC jobs require type-specific diagnostics that this rule does not attempt.

## Related

- [crdb_job_failed](crdb_job_failed.md)
- [crdb_job_reverting](crdb_job_reverting.md)
- [crdb_resource_pressure](crdb_resource_pressure.md)
