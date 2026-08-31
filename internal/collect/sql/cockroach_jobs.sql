SELECT job_id::STRING                                                    AS job_id,
       coalesce(job_type, '')                                            AS job_type,
       coalesce(state, '')                                               AS state,
       created                                                           AS created_at,
       finished                                                          AS finished_at,
       coalesce(progress_fraction, 0)::FLOAT8                            AS progress,
       progress_fraction IS NOT NULL                                     AS progress_known,
       coalesce(crdb_internal.redact(
         crdb_internal.redactable_sql_constants(coalesce(description, ''))
       ), '')                                                            AS operation,
       coalesce(crdb_internal.redact(coalesce(status_message, '')), '')  AS status_message,
       coalesce(crdb_internal.redact(coalesce(error, '')), '')           AS error,
       last_updated                                                      AS last_updated_at,
       CASE WHEN resolved IS NULL THEN NULL
            ELSE hlc_to_timestamp(resolved) END                          AS high_water_at,
       count(*) OVER ()::INT8                                            AS total_jobs
FROM information_schema.crdb_jobs_with_progress
WHERE state IN ('running', 'pending', 'paused', 'pause-requested',
                'cancel-requested', 'reverting', 'failed', 'revert-failed')
  AND (finished IS NULL OR finished >= now() - INTERVAL '24 hours')
ORDER BY CASE WHEN state IN ('failed', 'revert-failed') THEN 0
              WHEN state = 'reverting' THEN 1
              WHEN state IN ('pause-requested', 'cancel-requested') THEN 2
              WHEN state = 'paused' THEN 3 ELSE 4 END,
         created DESC
LIMIT 25;
