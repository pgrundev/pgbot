package render

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pgrundev/pgbot/internal/model"
)

// The default view is a graded, grouped read: a 0–100 health score, then the
// findings bucketed CRITICAL / WARNING / NOTE, then a GOOD list of the healthy
// subsystems named with their values. Fast to read top-to-bottom.

type statusKind int

const (
	kOK statusKind = iota
	kWatch
	kBad
	kInfo
)

func statusColor(st styler, k statusKind) func(string) string {
	switch k {
	case kOK:
		return st.good
	case kWatch:
		return st.warn
	case kBad:
		return st.crit
	default:
		return st.dim
	}
}

func renderGrouped(b *strings.Builder, st styler, c *model.Context, width int) {
	score := computeHealthScore(c)
	paintScore := st.good
	switch {
	case score < 70:
		paintScore = st.crit
	case score < 90:
		paintScore = st.warn
	}
	if c.Server.Engine == "cockroachdb" {
		clusterScore, workloadScore := computeCockroachScores(c)
		if cockroachClusterHealthAvailable(c) {
			fmt.Fprintf(b, "%s %s\n", st.dim("Cluster health:"), scorePaint(st, clusterScore)(fmt.Sprintf("%d/100", clusterScore)))
		} else {
			fmt.Fprintf(b, "%s %s\n", st.dim("Cluster health:"), st.dim("unavailable (configure --crdb-admin-url)"))
		}
		fmt.Fprintf(b, "%s %s\n", st.dim("Workload health:"), scorePaint(st, workloadScore)(fmt.Sprintf("%d/100", workloadScore)))
		if cockroachClusterHealthAvailable(c) {
			fmt.Fprintln(b, st.dim("Coverage: Admin API + Prometheus + jobs + SQL workload"))
		} else {
			fmt.Fprintln(b, st.dim("Coverage: SQL workload diagnostics (configure --crdb-admin-url for cluster health)"))
		}
		fmt.Fprintln(b)
	} else {
		// Under --profile=schema the score grades only the schema; label it so, and it
		// never reads as an overall "database health" verdict.
		scoreLabel := "Database health:"
		if c.Profile == "schema" {
			scoreLabel = "Schema check:"
		}
		fmt.Fprintf(b, "%s %s\n\n", st.dim(scoreLabel), paintScore(fmt.Sprintf("%d/100", score)))
	}

	var crit, warn, note []model.Finding
	hidden := 0      // suppressed non-criticals: counted in the footer, not listed
	preexisting := 0 // --fail-on-new: already in base, not this change's regressions
	for _, f := range c.Findings {
		// --fail-on-new (D3-2): show only what this change introduced. Preexisting
		// findings drop to a footer count (they remain in --json).
		if f.Preexisting {
			preexisting++
			continue
		}
		// A suppressed CRITICAL still renders (visibly marked) — a config must never
		// be able to make checksum_failures vanish from the screen (B2-2). Lesser
		// suppressed findings drop to the footer / --full.
		if f.Suppressed && f.Severity != model.SeverityCritical {
			hidden++
			continue
		}
		switch f.Severity {
		case model.SeverityCritical:
			crit = append(crit, f)
		case model.SeverityWarn:
			warn = append(warn, f)
		default:
			note = append(note, f)
		}
	}
	byImpact := func(fs []model.Finding) {
		sort.SliceStable(fs, func(i, j int) bool { return fs[i].Impact.Score > fs[j].Impact.Score })
	}
	byImpact(crit)
	byImpact(warn)

	// emit one severity group: a colored header, then a bullet per finding title.
	emit := func(header string, paint func(string) string, fs []model.Finding) {
		if len(fs) == 0 {
			return
		}
		fmt.Fprintln(b, paint(header))
		for _, f := range fs {
			lines := wrapText(f.Title, width-4)
			fmt.Fprintf(b, "%s %s\n", paint("●"), lines[0])
			for _, l := range lines[1:] {
				fmt.Fprintf(b, "  %s\n", l)
			}
			if f.Suppressed { // a rendered-but-suppressed critical
				fmt.Fprintf(b, "  %s\n", st.dim("suppressed by config: "+suppReason(f)))
			}
			// A destructive-action guard travels even in the compact view — a
			// prohibition or precondition is exactly what must not be dropped for space.
			renderSafetyGuards(b, st, f, width, "  ")
		}
		fmt.Fprintln(b)
	}
	emit("CRITICAL", st.crit, crit)
	emit("WARNING", st.warn, warn)
	emit("NOTE", st.info, note)

	if hidden > 0 {
		fmt.Fprintln(b, st.dim(fmt.Sprintf("%d finding(s) suppressed by config (see --full)", hidden)))
		fmt.Fprintln(b)
	}
	if preexisting > 0 {
		fmt.Fprintln(b, st.dim(fmt.Sprintf("%d finding(s) already present in the base — not introduced by this change (see --json)", preexisting)))
		fmt.Fprintln(b)
	}

	if c.Server.Engine == "cockroachdb" {
		renderCockroachCompactHealth(b, st, c)
		renderCockroachCompactDistribution(b, st, c)
		renderCockroachCompactStorage(b, st, c)
		renderCockroachCompactJobs(b, st, c)
	}
	if c.Server.Engine == "cockroachdb" && c.Activity != nil && c.Activity.Exactness != model.ExactnessUnavailable {
		a := c.Activity
		fmt.Fprintln(b, st.head("ACTIVITY"))
		fmt.Fprintf(b, "  %d total · %d active · %d idle\n", a.Total, a.Active, a.Idle)
		if a.LongestActiveSec > 0 {
			fmt.Fprintf(b, "  longest active query %.0fs\n", a.LongestActiveSec)
		}
		fmt.Fprintln(b)
	}
	if c.Server.Engine == "cockroachdb" {
		renderCockroachCompactWorkload(b, st, c)
	}

	// The GOOD list infers health from the ABSENCE of a finding — valid only when
	// the check actually ran. A schema profile skips the workload/infra collectors,
	// so "no blocking locks" there would be a claim about a database it never
	// examined. Suppress it; the header already says this is schema-only.
	if c.Profile != "schema" {
		if good := buildGood(c); len(good) > 0 {
			fmt.Fprintln(b, st.good("GOOD"))
			for _, g := range good {
				fmt.Fprintf(b, "%s %s\n", st.good("●"), st.dim(g))
			}
			fmt.Fprintln(b)
		}
	}

	fmt.Fprintln(b, st.dim("Details: pgbot inspect --full   ·   Machine-readable: --json"))
	fmt.Fprintln(b, st.dim(`Ask it: pgbot ask "what's wrong?"`))
}

func renderCockroachCompactDistribution(b *strings.Builder, st styler, c *model.Context) {
	if c.Health == nil || c.Health.Cockroach == nil {
		return
	}
	d := &c.Health.Cockroach.Distribution
	if d.Exactness == "" || d.Exactness == model.ExactnessUnavailable {
		return
	}
	note := fmt.Sprintf("%d/%d live stores comparable", d.ComparableStores, d.LiveStores)
	if d.Reason != "" {
		note += " · partial"
	}
	fmt.Fprintf(b, "%s  %s\n", st.head("DISTRIBUTION & BALANCE"), st.dim(note))
	fmt.Fprintf(b, "  replicas %d–%d · leases %d–%d · utilization %s–%s\n",
		d.ReplicaMin, d.ReplicaMax, d.LeaseMin, d.LeaseMax, pct(d.CapacityUsedMinRatio), pct(d.CapacityUsedMaxRatio))
	if d.HotRangeLeaseholderSamples > 0 {
		fmt.Fprintf(b, "  top-hot leader n%d · %d/%d ranges · %.1f%% sampled CPU\n",
			d.HottestLeaseholderNodeID, d.HottestLeaseholderRanges, d.HotRangeLeaseholderSamples, d.HottestLeaseholderCPUShare*100)
	}
	fmt.Fprintln(b)
}

func renderCockroachCompactStorage(b *strings.Builder, st styler, c *model.Context) {
	if c.Health == nil || c.Health.Cockroach == nil {
		return
	}
	s := &c.Health.Cockroach.Storage
	if s.Exactness == "" || s.Exactness == model.ExactnessUnavailable {
		return
	}
	note := s.Exactness
	if s.Reason != "" {
		note += " · partial"
	}
	fmt.Fprintf(b, "%s  %s\n", st.head("STORAGE & REPLICATION"), st.dim(note))
	fmt.Fprintf(b, "  filesystem %s used · CockroachDB %s · other use/overhead %s\n",
		humanBytes(s.FilesystemUsedBytes), humanBytes(s.CockroachUsedBytes), humanBytes(s.OtherUsedBytes))
	if s.MVCCMetricsAvailable {
		fmt.Fprintf(b, "  MVCC %s live / %s total (%s) · %s–%s per replica\n",
			humanBytes(s.MVCCLiveBytes), humanBytes(s.MVCCTotalBytes), pct(s.MVCCLiveRatio),
			humanBytes(int64(s.BytesPerReplicaMin)), humanBytes(int64(s.BytesPerReplicaMax)))
	}
	if s.ReplicationMetricsAvailable {
		fmt.Fprintf(b, "  recovery %s uninitialized · %s reserved · queue %s pending / %s purgatory\n",
			humanNum(float64(s.UninitializedReplicas)), humanNum(float64(s.ReservedReplicas)),
			humanNum(float64(s.ReplicateQueuePending)), humanNum(float64(s.ReplicateQueuePurgatory)))
		fmt.Fprintf(b, "  Raft %s commands pending · %s probe / %s snapshot flows\n",
			humanNum(float64(s.RaftCommandsPending)), humanNum(float64(s.RaftProbeFlows)), humanNum(float64(s.RaftSnapshotFlows)))
	}
	if s.CounterSampledStores > 0 {
		fmt.Fprintf(b, "  %.2fs I/O sample · %d slow · %d stalled · %d write stalls · %d Raft drops\n",
			s.SampleSeconds, s.DiskSlowEvents, s.DiskStalledEvents, s.WriteStallEvents, s.RaftDroppedMessages)
	}
	fmt.Fprintln(b)
}

func renderCockroachCompactJobs(b *strings.Builder, st styler, c *model.Context) {
	if c.Health == nil || c.Health.Cockroach == nil {
		return
	}
	h := c.Health.Cockroach
	if h.Jobs.Exactness == "" || h.Jobs.Exactness == model.ExactnessUnavailable {
		return
	}
	counts := countCockroachJobs(h.JobItems)
	fmt.Fprintf(b, "%s  %s\n", st.head("JOBS & SCHEMA CHANGES"), st.dim(h.Jobs.Exactness))
	if len(h.JobItems) == 0 {
		fmt.Fprintln(b, "  no active, paused, reverting, or recently failed jobs")
		fmt.Fprintln(b)
		return
	}
	fmt.Fprintf(b, "  %d active · %d paused · %d reverting · %d failed",
		counts.active, counts.paused, counts.reverting, counts.failed)
	if h.JobsBounded {
		fmt.Fprintf(b, " · showing %d/%d", len(h.JobItems), h.JobsTotal)
	}
	fmt.Fprintln(b)
	for _, j := range h.JobItems[:min(3, len(h.JobItems))] {
		fmt.Fprintf(b, "  job %s · %s · %s · %s · age %s\n",
			j.JobID, truncate(j.Type, 26), j.State, cockroachJobProgress(j), jobAge(c.CollectedAt, j.CreatedAt))
		if j.Operation != "" {
			fmt.Fprintf(b, "    %s\n", truncate(j.Operation, 72))
		}
	}
	fmt.Fprintln(b)
}

func renderCockroachCompactHealth(b *strings.Builder, st styler, c *model.Context) {
	if !cockroachClusterHealthAvailable(c) {
		return
	}
	h := c.Health.Cockroach
	fmt.Fprintln(b, st.head("CLUSTER HEALTH"))
	fmt.Fprintf(b, "  %d/%d nodes live · %d stores · %d unavailable · %d under-replicated\n",
		h.NodesLive, h.NodesTotal, h.StoresTotal, h.UnavailableRanges, h.UnderreplicatedRanges)
	if h.CapacityBytes > 0 {
		fmt.Fprintf(b, "  %s free of %s · fullest store %s\n",
			humanBytes(h.AvailableBytes), humanBytes(h.CapacityBytes), pct(h.MaxStoreUsedRatio))
	}
	parts := []string{}
	if h.QueriesPerSec != nil {
		parts = append(parts, humanNum(*h.QueriesPerSec)+" queries/s")
	}
	parts = append(parts, fmt.Sprintf("%d SQL connections", h.SQLConnections))
	if h.MaxCPUPercent > 0 {
		parts = append(parts, fmt.Sprintf("peak CPU %.1f%%", h.MaxCPUPercent))
	}
	if len(parts) > 0 {
		fmt.Fprintln(b, "  "+strings.Join(parts, " · "))
	}
	fmt.Fprintln(b)
}

func renderCockroachCompactWorkload(b *strings.Builder, st styler, c *model.Context) {
	if c.Cockroach != nil && c.Cockroach.LiveQueries.Exactness != model.ExactnessUnavailable && len(c.Cockroach.LiveQueries.Items) > 0 {
		fmt.Fprintln(b, st.head("LIVE QUERIES"))
		shown := min(3, len(c.Cockroach.LiveQueries.Items))
		for _, q := range c.Cockroach.LiveQueries.Items[:shown] {
			flags := ""
			if q.FullScan {
				flags = " · full scan"
			}
			if q.Retries > 0 {
				flags += fmt.Sprintf(" · %d retries", q.Retries)
			}
			fmt.Fprintf(b, "  %.0fs · %s · %s%s\n", q.AgeSec, orNone(q.AppName), truncate(q.Query, 58), flags)
		}
		fmt.Fprintln(b)
	}
	renderCockroachCompactContention(b, st, c)
	renderCockroachCompactTables(b, st, c)
	renderCockroachCompactIndexes(b, st, c)
	if c.Queries == nil || !c.Queries.Enabled || c.Queries.Exactness == model.ExactnessUnavailable {
		return
	}
	fmt.Fprintln(b, st.head(cockroachQueryHeading(c.Queries)))
	if len(c.Queries.Top) == 0 {
		fmt.Fprintln(b, st.dim("  no persisted statement statistics have flushed yet"))
		fmt.Fprintln(b)
		return
	}
	shown := min(3, len(c.Queries.Top))
	for _, q := range c.Queries.Top[:shown] {
		fmt.Fprintf(b, "  %s · mean %.1fms · max p99 %s · max retries %d · %s\n",
			orNone(q.AppName), q.MeanMS, formatOptionalMS(q.P99MS, 1), q.MaxRetries, truncate(q.Query, 52))
	}
	fmt.Fprintln(b)
}

func renderCockroachCompactIndexes(b *strings.Builder, st styler, c *model.Context) {
	if c.Indexes == nil || c.Indexes.Exactness == model.ExactnessUnavailable {
		return
	}
	threshold := indexThreshold(c.Indexes)
	fmt.Fprintf(b, "%s  %s\n", st.head("INDEX USAGE"), st.dim("cluster-wide · in-memory"))
	fmt.Fprintf(b, "  %d total · %d secondary · %d unused ≥%s\n",
		c.Indexes.Total, c.Indexes.SecondaryTotal, len(c.Indexes.Unused), threshold)
	for _, ix := range c.Indexes.Unused[:min(3, len(c.Indexes.Unused))] {
		fmt.Fprintf(b, "  %s · no reads for %s", truncate(indexObject(ix), 54), indexUnusedAge(ix))
		if c.Indexes.WriteCountersAvailable {
			fmt.Fprintf(b, " · %s writes", humanNum(float64(ix.Writes)))
		}
		fmt.Fprintln(b)
	}
	if !c.Indexes.WriteCountersAvailable {
		fmt.Fprintln(b, st.dim("  write counters unavailable on this CockroachDB version"))
	}
	fmt.Fprintln(b)
}

func renderCockroachCompactTables(b *strings.Builder, st styler, c *model.Context) {
	if c.Tables == nil || c.Tables.Exactness == model.ExactnessUnavailable {
		return
	}
	cacheAge := tableMetadataAge(c)
	headingNote := "cached Admin API metadata"
	if cacheAge != "unknown" {
		headingNote += " · oldest " + cacheAge
	}
	fmt.Fprintf(b, "%s  %s\n", st.head("TABLE HEALTH"), st.dim(headingNote))
	fmt.Fprintf(b, "  %d tables · %s replicated disk estimate\n", c.Tables.Total, humanBytes(c.Tables.DBSizeBytes))
	for _, table := range c.Tables.Top[:min(3, len(c.Tables.Top))] {
		fmt.Fprintf(b, "  %s · %s · %s live · %s ranges",
			truncate(tableObject(table), 48), humanBytes(table.ReplicatedBytes), tableLivePercent(table), humanNum(float64(table.RangeCount)))
		if table.StatsLastUpdated == nil {
			fmt.Fprint(b, " · stats missing")
		} else {
			fmt.Fprintf(b, " · stats %s", ageSince(c.CollectedAt, table.StatsLastUpdated))
		}
		if table.TopHotRangeCount > 0 {
			fmt.Fprintf(b, " · %d top hot", table.TopHotRangeCount)
		}
		fmt.Fprintln(b)
	}
	fmt.Fprintln(b)
}

func renderCockroachCompactContention(b *strings.Builder, st styler, c *model.Context) {
	if c.Cockroach == nil || c.Cockroach.Contention.Exactness == "" || c.Cockroach.Contention.Exactness == model.ExactnessUnavailable {
		return
	}
	h := &c.Cockroach.Contention
	fmt.Fprintf(b, "%s  %s\n", st.head("CONTENTION"), st.dim(fmt.Sprintf("last %dm · bounded event store", h.WindowMinutes)))
	if h.TotalEvents == 0 {
		fmt.Fprintln(b, "  no recorded contention events")
		fmt.Fprintln(b)
		return
	}
	fmt.Fprintf(b, "  %d events · %s total wait · %s longest", h.TotalEvents, humanDurationMS(h.TotalWaitMS), humanDurationMS(h.MaxWaitMS))
	if h.SerializationConflicts > 0 {
		fmt.Fprintf(b, " · %d serialization conflicts", h.SerializationConflicts)
	}
	fmt.Fprintln(b)
	for _, x := range h.Hotspots[:min(3, len(h.Hotspots))] {
		fmt.Fprintf(b, "  %s · %d events · %s total · waiter q:%s",
			truncate(contentionObject(x), 48), x.Events, humanDurationMS(x.TotalWaitMS), shortFingerprint(x.WaitingStatementFingerprint))
		if x.BlockingTxnFingerprint != "" {
			fmt.Fprintf(b, " · blocker txn:%s", shortFingerprint(x.BlockingTxnFingerprint))
		}
		fmt.Fprintln(b)
		fmt.Fprintf(b, "    %s\n", contentionWaiterSummary(x, 64))
		fmt.Fprintf(b, "    %s\n", contentionBlockerSummary(x, 64))
	}
	fmt.Fprintln(b)
}

// renderSafetyGuards prints a finding's structured destructive-action guards
// compactly — a prohibition as "do NOT <ACTION>", a precondition as "before
// <ACTION>, confirm: <verify>". Guaranteed from the structured field, never
// summary prose, and shared by the grouped view and the --full findings list.
func renderSafetyGuards(b *strings.Builder, st styler, f model.Finding, width int, indent string) {
	if f.Safety == nil {
		return
	}
	for _, g := range f.Safety.BlockingCaveats {
		var line string
		switch {
		case g.Kind == model.GuardProhibition:
			line = "⚠ do NOT " + g.Action + " — " + g.Text
		case g.Verify != nil:
			line = "⚠ before " + g.Action + ", confirm: " + *g.Verify
		default:
			line = "⚠ " + g.Action + ": " + g.Text
		}
		for _, l := range wrapText(line, width-len(indent)-2) {
			fmt.Fprintf(b, "%s%s\n", indent, st.warn(l))
		}
	}
}

// safetyText renders a finding's destructive-action guards as one plain-text line,
// for the text-message surfaces (SARIF, JUnit). Empty when there are no guards.
func safetyText(f *model.Finding) string {
	if f == nil || f.Safety == nil || len(f.Safety.BlockingCaveats) == 0 {
		return ""
	}
	var parts []string
	for _, g := range f.Safety.BlockingCaveats {
		s := "SAFETY [" + g.Action + "]: " + g.Text
		if g.Verify != nil {
			s += " Only after: " + *g.Verify
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, " ")
}

// computeHealthScore is a coarse 0–100 grade: full marks minus a penalty per
// finding by severity. It's an at-a-glance indicator, not a precise metric.
// suppReason renders an ignore rule's reason, or a stand-in when none was given.
func suppReason(f model.Finding) string {
	if f.SuppressionReason != "" {
		return f.SuppressionReason
	}
	return "no reason given"
}

func computeHealthScore(c *model.Context) int {
	penalty := 0
	for _, f := range c.Findings {
		penalty += findingPenalty(f)
	}
	return scoreAfterPenalty(penalty)
}

func computeCockroachScores(c *model.Context) (cluster, workload int) {
	clusterPenalty, workloadPenalty := 0, 0
	for _, f := range c.Findings {
		penalty := findingPenalty(f)
		if cockroachClusterFinding(f.ID) {
			clusterPenalty += penalty
		} else {
			workloadPenalty += penalty
		}
	}
	return scoreAfterPenalty(clusterPenalty), scoreAfterPenalty(workloadPenalty)
}

// cockroachClusterFinding is deliberately narrower than the finding catalogue's
// infra scope. The headline cluster score describes serving infrastructure;
// operational SQL jobs belong with workload health even though they are
// cluster-wide and classified as infra for --profile=schema filtering.
func cockroachClusterFinding(id string) bool {
	switch id {
	case "crdb_node_unavailable", "crdb_ranges_unavailable", "crdb_ranges_underreplicated",
		"crdb_store_capacity", "crdb_resource_pressure", "crdb_version_skew",
		"crdb_replica_imbalance", "crdb_leaseholder_imbalance", "crdb_capacity_imbalance",
		"crdb_external_disk_usage", "crdb_storage_stall", "crdb_replication_recovery",
		"crdb_raft_backlog", "crdb_replica_size_skew":
		return true
	default:
		return false
	}
}

func findingPenalty(f model.Finding) int {
	if f.Suppressed {
		return 0 // a muted finding shouldn't drag the grade (B2-2)
	}
	switch f.Severity {
	case model.SeverityCritical:
		return 10
	case model.SeverityWarn:
		return 3
	default:
		return 1
	}
}

func scoreAfterPenalty(penalty int) int {
	if score := 100 - penalty; score > 0 {
		return score
	}
	return 0
}

func scorePaint(st styler, score int) func(string) string {
	switch {
	case score < 70:
		return st.crit
	case score < 90:
		return st.warn
	default:
		return st.good
	}
}

// buildGood names the subsystems pgbot checked and found healthy, with their
// values — the "a colleague who looked" signal. Only names things actually
// examined and clean, capped so the list stays scannable.
func buildGood(c *model.Context) []string {
	fired := map[string]bool{}
	for _, f := range c.Findings {
		fired[f.ID] = true
	}
	var g []string
	if c.Server.Engine == "cockroachdb" && cockroachClusterHealthAvailable(c) {
		h := c.Health.Cockroach
		if !fired["crdb_node_unavailable"] && !fired["crdb_ranges_unavailable"] && !fired["crdb_ranges_underreplicated"] {
			g = append(g, fmt.Sprintf("all %d nodes live; no unavailable or under-replicated ranges", h.NodesTotal))
		}
		if h.CapacityBytes > 0 && !fired["crdb_store_capacity"] {
			g = append(g, fmt.Sprintf("capacity healthy; fullest store %s", pct(h.MaxStoreUsedRatio)))
		}
		if !fired["crdb_resource_pressure"] && h.MaxCPUPercent > 0 {
			g = append(g, fmt.Sprintf("no resource pressure; peak CPU %.1f%%", h.MaxCPUPercent))
		}
	}
	if h := c.Health; h != nil && h.Exactness != model.ExactnessUnavailable {
		if h.CacheHitUsable() && !fired["low_cache_hit"] {
			g = append(g, fmt.Sprintf("cache hit ratio %.1f%%", *h.CacheHitRatio*100))
		}
		if h.DeadlocksPerMin != nil && *h.DeadlocksPerMin == 0 {
			g = append(g, "no deadlocks")
		}
	}
	if c.Locks != nil && c.Locks.Exactness != model.ExactnessUnavailable && c.Locks.BlockedCount == 0 {
		g = append(g, "no blocking locks")
	}
	if r := c.Replication; r != nil && r.Exactness != model.ExactnessUnavailable {
		switch {
		case r.IsReplica:
			g = append(g, "replication healthy (replica)")
		case len(r.Replicas) > 0:
			g = append(g, fmt.Sprintf("replication healthy (%d streaming)", len(r.Replicas)))
		}
	}
	if c.Schema != nil && !fired["index_invalid"] {
		g = append(g, "no invalid indexes")
	}
	if c.Server.Engine == "cockroachdb" && c.Indexes != nil && c.Indexes.Exactness != model.ExactnessUnavailable && !fired["crdb_unused_indexes"] {
		g = append(g, "no secondary indexes unused for "+indexThreshold(c.Indexes))
	}
	if c.Tables != nil && c.Tables.Exactness != model.ExactnessUnavailable {
		if c.Server.Engine == "cockroachdb" {
			clean := !fired["crdb_table_metadata_error"] && !fired["crdb_table_stats_missing"] && !fired["crdb_auto_stats_disabled"] && !fired["crdb_mvcc_garbage_pressure"]
			if clean {
				g = append(g, fmt.Sprintf("table metadata healthy across %d tables", c.Tables.Total))
			}
		} else if !fired["table_bloat"] {
			g = append(g, "no significant table bloat")
		}
	}
	if c.Limits != nil && c.Limits.Exactness != model.ExactnessUnavailable {
		if !fired["txid_wraparound"] && c.Limits.MaxXIDAge > 0 {
			g = append(g, "no wraparound risk")
		}
		if !fired["connection_saturation"] && c.Limits.ConnectionsMax > 0 {
			g = append(g, fmt.Sprintf("connections %d/%d", c.Limits.ConnectionsUsed, c.Limits.ConnectionsMax))
		}
	}
	if c.Queries != nil && c.Queries.Enabled && !fired["pg_stat_statements_missing"] {
		g = append(g, "query stats available")
	}
	if c.Server.Engine == "cockroachdb" && c.Cockroach != nil && c.Cockroach.Contention.Exactness != model.ExactnessUnavailable && c.Cockroach.Contention.TotalEvents == 0 {
		g = append(g, "no contention events recorded in the last hour")
	}
	if len(g) > 6 {
		g = g[:6]
	}
	return g
}

// pgLower renders "postgres 16.3" for the header.
func pgLower(num int) string {
	if num == 0 {
		return "postgres"
	}
	return fmt.Sprintf("postgres %d.%d", num/10000, num%100)
}

func serverLabel(s model.ServerInfo) string {
	if s.Engine != "cockroachdb" {
		return pgLower(s.VersionNum)
	}
	fields := strings.Fields(s.VersionText)
	for _, f := range fields {
		if strings.HasPrefix(strings.ToLower(f), "v") && len(f) > 1 {
			return "cockroachdb " + f
		}
	}
	return "cockroachdb"
}
