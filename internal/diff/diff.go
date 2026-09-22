// Package diff turns a current Context plus a stored baseline into
// model.Deltas — the "what changed and why it matters" layer that makes pgbot
// more than a stats reader. Thresholds are materiality gates so we surface real
// movement, not float noise. This is also the raw material for `pgbot why`.
package diff

import (
	"fmt"
	"time"

	"github.com/pgrundev/pgbot/internal/model"
)

// Baseline is the minimal shape diff needs from a stored snapshot.
type Baseline struct {
	CollectedAt time.Time
	Context     *model.Context
}

// Materiality thresholds.
const (
	queryMeanPct     = 0.50 // ±50%
	queryMeanAbsMS   = 5.0  // and at least 5ms absolute
	seqScanFactor    = 2.0  // cumulative seq scans more than doubled
	seqScanAbsMin    = 1000
	dbGrowthPct      = 0.10
	dbGrowthAbsBytes = 100 << 20
	connStepPct      = 0.50
	connStepAbsMin   = 20
	deadRatioStep    = 0.10 // +10 percentage points
	replLagStepSec   = 30
)

// Compute compares now against the primary baseline (yesterday is accepted for
// FirstObserved dating but the primary drives the changes). Returns nil when
// there's nothing to compare against.
func Compute(now *model.Context, primary *Baseline, _ *Baseline) *model.Deltas {
	if primary == nil || primary.Context == nil {
		return nil
	}
	d := &model.Deltas{Against: primary.CollectedAt, Changes: []model.Delta{}}
	base := primary.Context

	queryDeltas(now, base, primary.CollectedAt, d)
	tableSeqScanDeltas(now, base, primary.CollectedAt, d)
	dbSizeDelta(now, base, primary.CollectedAt, d)
	connectionDelta(now, base, primary.CollectedAt, d)
	deadRatioDelta(now, base, primary.CollectedAt, d)
	replicationDelta(now, base, primary.CollectedAt, d)
	archiverFailedDelta(now, base, primary.CollectedAt, d)
	replicaGoneDelta(now, base, primary.CollectedAt, d)
	return d
}

// replicaGoneDelta flags a standby that was streaming at the baseline but is
// absent from pg_stat_replication now — a silent disconnect only history sees.
func replicaGoneDelta(now, base *model.Context, baseAt time.Time, d *model.Deltas) {
	if !replicationObserved(now.Replication) || !replicationObserved(base.Replication) {
		return
	}
	present := map[string]bool{}
	for _, r := range now.Replication.Replicas {
		present[standbyKey(r)] = true
	}
	for _, r := range base.Replication.Replicas {
		if k := standbyKey(r); k != "" && !present[k] {
			at := baseAt
			d.Changes = append(d.Changes, model.Delta{
				ID: "replication.standby_gone", Subject: k, Severity: model.SeverityWarn,
				Before: 1, After: 0, FirstObserved: &at,
				Note: "a standby present at the last run is no longer connected",
			})
		}
	}
}

func standbyKey(r model.ReplicaRow) string {
	if r.AppName != "" {
		return r.AppName
	}
	return r.ClientAddr
}

// archiverFailedDelta reports NEW WAL-archiving failures since the baseline — the
// run-over-run signal that catches intermittent archiving failure, which a
// point-in-time last_failed_time < last_archived_time comparison misses. Only a
// genuine increase (not a counter reset going backwards) is reported.
func archiverFailedDelta(now, base *model.Context, baseAt time.Time, d *model.Deltas) {
	if !archiverObserved(now.Archiver) || !archiverObserved(base.Archiver) {
		return
	}
	if inc := now.Archiver.FailedCount - base.Archiver.FailedCount; inc > 0 {
		at := baseAt
		d.Changes = append(d.Changes, model.Delta{
			ID: "archiver.failed_count", Subject: "archiver", Severity: model.SeverityWarn,
			Before: float64(base.Archiver.FailedCount), After: float64(now.Archiver.FailedCount),
			FirstObserved: &at,
			Note:          fmt.Sprintf("%d new archiving failures since the last run", inc),
		})
	}
}

func queryDeltas(now, base *model.Context, baseAt time.Time, d *model.Deltas) {
	if !queriesObserved(now.Queries) || !queriesObserved(base.Queries) {
		return
	}
	baseByID := map[int64]model.QueryStat{}
	for _, q := range base.Queries.Top {
		baseByID[q.QueryID] = q
	}
	for _, q := range now.Queries.Top {
		prev, seen := baseByID[q.QueryID]
		if !seen {
			// A query that wasn't in the previous top-20 by total time.
			at := now.CollectedAt
			d.Changes = append(d.Changes, model.Delta{
				ID: "query.new", Subject: shortID(q.QueryID), Severity: model.SeverityInfo,
				Before: 0, After: round1(q.TotalMS), FirstObserved: &at,
				Note: "new entry in top queries by total time",
			})
			continue
		}
		if prev.MeanMS <= 0 {
			continue
		}
		change := q.MeanMS - prev.MeanMS
		pct := change / prev.MeanMS
		if abs(change) >= queryMeanAbsMS && abs(pct) >= queryMeanPct {
			sev := model.SeverityInfo
			if change > 0 {
				sev = model.SeverityWarn // slower is the one you want to see
			}
			at := baseAt
			p := round4(pct)
			d.Changes = append(d.Changes, model.Delta{
				ID: "query.mean_ms", Subject: shortID(q.QueryID), Severity: sev,
				Before: round4(prev.MeanMS), After: round4(q.MeanMS), PctChange: &p,
				FirstObserved: &at, Note: "mean execution time per call",
			})
		}
	}
}

func tableSeqScanDeltas(now, base *model.Context, baseAt time.Time, d *model.Deltas) {
	if now.Tables == nil || base.Tables == nil {
		return
	}
	prev := map[string]int64{}
	for _, t := range base.Tables.Top {
		prev[t.Schema+"."+t.Name] = t.SeqScans
	}
	for _, t := range now.Tables.Top {
		before, ok := prev[t.Schema+"."+t.Name]
		if !ok {
			continue
		}
		grew := t.SeqScans - before
		if before >= 0 && float64(t.SeqScans) > float64(before)*seqScanFactor && grew >= seqScanAbsMin {
			at := baseAt
			p := pctOrNil(float64(before), float64(t.SeqScans))
			d.Changes = append(d.Changes, model.Delta{
				ID: "table.seq_scans", Subject: t.Schema + "." + t.Name, Severity: model.SeverityWarn,
				Before: float64(before), After: float64(t.SeqScans), PctChange: p,
				FirstObserved: &at, Note: "sequential scans surged — a query may have lost an index path",
			})
		}
	}
}

func dbSizeDelta(now, base *model.Context, baseAt time.Time, d *model.Deltas) {
	if now.Tables == nil || base.Tables == nil {
		return
	}
	before, after := base.Tables.DBSizeBytes, now.Tables.DBSizeBytes
	grew := after - before
	if before > 0 && grew >= dbGrowthAbsBytes && float64(grew)/float64(before) >= dbGrowthPct {
		at := baseAt
		p := pctOrNil(float64(before), float64(after))
		d.Changes = append(d.Changes, model.Delta{
			ID: "database.size_bytes", Subject: now.Server.Database, Severity: model.SeverityInfo,
			Before: float64(before), After: float64(after), PctChange: p,
			FirstObserved: &at, Note: "database grew notably since the baseline",
		})
	}
}

func connectionDelta(now, base *model.Context, baseAt time.Time, d *model.Deltas) {
	if !connectionsObserved(now.Health) || !connectionsObserved(base.Health) {
		return
	}
	before, after := base.Health.Connections, now.Health.Connections
	step := after - before
	if before > 0 && abs(float64(step)) >= connStepAbsMin && abs(float64(step))/float64(before) >= connStepPct {
		at := baseAt
		p := pctOrNil(float64(before), float64(after))
		sev := model.SeverityInfo
		if step > 0 {
			sev = model.SeverityWarn
		}
		d.Changes = append(d.Changes, model.Delta{
			ID: "health.connections", Subject: now.Server.Database, Severity: sev,
			Before: float64(before), After: float64(after), PctChange: p,
			FirstObserved: &at, Note: "connection count stepped",
		})
	}
}

// Older stored contexts can lack exactness. Preserve their positive evidence,
// but never turn an all-default legacy section into a confidently empty sample.
func replicationObserved(r *model.Replication) bool {
	if r == nil || r.Exactness == model.ExactnessUnavailable {
		return false
	}
	return r.Exactness != "" || r.IsReplica || r.ReceiverLagSec != nil ||
		len(r.Replicas) > 0 || len(r.Slots) > 0 || len(r.Subscriptions) > 0
}

func archiverObserved(a *model.Archiver) bool {
	if a == nil || a.Exactness == model.ExactnessUnavailable {
		return false
	}
	return a.Exactness != "" || a.ArchivedCount != 0 || a.LastArchivedWAL != "" ||
		a.LastArchivedTime != nil || a.FailedCount != 0 || a.LastFailedWAL != "" ||
		a.LastFailedTime != nil || a.StatsReset != nil || a.HasArchiveCommand
}

func queriesObserved(q *model.Queries) bool {
	if q == nil || !q.Enabled || q.Exactness == model.ExactnessUnavailable {
		return false
	}
	return q.Exactness != "" || q.TotalExecMS != 0 || q.PgssDealloc != 0 ||
		q.PgssCount != 0 || q.PgssMax != 0 || len(q.Top) > 0
}

func connectionsObserved(h *model.Health) bool {
	if h == nil {
		return false
	}
	if h.Connections > 0 {
		return true
	}
	if h.Exactness == model.ExactnessUnavailable {
		return false
	}
	return h.Exactness != "" || h.TPS != nil || h.CommitsPerSec != nil ||
		h.RollbacksPerSec != nil || h.RollbackRatio != nil || h.CacheHitRatio != nil ||
		h.CacheBlocks != nil || h.DeadlocksPerMin != nil || h.TempBytesPerSec != nil ||
		h.TupReturnedPerS != nil || h.TupWrittenPerS != nil
}

func deadRatioDelta(now, base *model.Context, baseAt time.Time, d *model.Deltas) {
	if now.Tables == nil || base.Tables == nil {
		return
	}
	prev := map[string]float64{}
	for _, t := range base.Tables.Top {
		prev[t.Schema+"."+t.Name] = t.DeadRatio
	}
	for _, t := range now.Tables.Top {
		before, ok := prev[t.Schema+"."+t.Name]
		if !ok {
			continue
		}
		if t.DeadRatio-before >= deadRatioStep {
			at := baseAt
			d.Changes = append(d.Changes, model.Delta{
				ID: "table.dead_ratio", Subject: t.Schema + "." + t.Name, Severity: model.SeverityWarn,
				Before: round4(before), After: round4(t.DeadRatio),
				FirstObserved: &at, Note: "dead-tuple ratio climbed — vacuum may be falling behind",
			})
		}
	}
}

func replicationDelta(now, base *model.Context, baseAt time.Time, d *model.Deltas) {
	if now.Replication == nil || base.Replication == nil {
		return
	}
	if now.Replication.ReceiverLagSec == nil || base.Replication.ReceiverLagSec == nil {
		return
	}
	before, after := *base.Replication.ReceiverLagSec, *now.Replication.ReceiverLagSec
	if after-before >= replLagStepSec {
		at := baseAt
		d.Changes = append(d.Changes, model.Delta{
			ID: "replication.receiver_lag_sec", Subject: now.Server.Database, Severity: model.SeverityWarn,
			Before: round2(before), After: round2(after), FirstObserved: &at,
			Note: "replica replay lag increased",
		})
	}
}
