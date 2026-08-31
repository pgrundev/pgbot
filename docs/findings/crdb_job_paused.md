---
id: crdb_job_paused
severity: info
critical_when: ""
dimension: risk
object: job
scope: infra
requires: [information_schema.crdb_jobs_with_progress]
thresholds: []
related: [crdb_job_stalled]
---

# crdb_job_paused

**Severity:** info · **Dimension:** risk · **Object identity:** `job`

## What pgbot observed

A background job is paused and will not make progress until it is resumed or canceled.

## Why it matters

The pause may be intentional, particularly for a changefeed, but it can also leave a schema change, backup, restore, import, or maintenance workflow incomplete.

## How to verify it yourself

```sql
SELECT job_id, job_type, state, status_message, last_updated
FROM information_schema.crdb_jobs_with_progress
WHERE state = 'paused';
```

## How to fix it

Confirm the job has an owner and that the pause is intentional. Resume or cancel it only after checking the operational consequences for that job type.

## When to ignore it

An intentionally paused job with a documented owner and resumption or retirement plan is informational.

```toml
[[ignore]]
finding = "crdb_job_paused"
object  = "job:<job-id>"
reason = "changefeed intentionally paused during downstream maintenance"
expires = "2027-01-01"
```

## What pgbot cannot see

pgbot cannot infer operator intent or determine whether downstream consumers still depend on the paused job.

## Related

- [crdb_job_stalled](crdb_job_stalled.md)
