package model

// SchemaVersion is the semver of the Context JSON contract. Agents and scripts
// pin against it, so bump the MAJOR on any breaking field change (removed or
// renamed field, changed type or units), the MINOR on additive fields.
//
// 1.1.0 (Phase 1): additive only — Events, WaitProfile, the T2 window/suppression
// fields, and Finding.Impact/Confidence/Caveats. No v1.0.0 field changed type or
// meaning, so a 1.0.0 consumer still parses 1.1.0 output (it ignores the new keys).
//
// 1.2.0: additive only — IndexStat gains columns/method/unique/primary (feeding
// index/code correlation). A 1.1.0 consumer still parses 1.2.0 output.
//
// 1.3.0: additive only — CockroachDB engine detection; cluster, workload,
// contention, index, table, job, distribution, and storage diagnostics; and
// the supporting health, attribution, freshness, and exactness fields.
const SchemaVersion = "1.3.0"
