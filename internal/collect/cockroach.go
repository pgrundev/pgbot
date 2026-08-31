package collect

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/crdbhttp"
	"github.com/pgrundev/pgbot/internal/model"
)

//go:embed sql/cockroach_live_queries.sql
var sqlCockroachLiveQueries string

//go:embed sql/cockroach_statement_insights.sql
var sqlCockroachStatementInsights string

//go:embed sql/cockroach_transaction_insights.sql
var sqlCockroachTransactionInsights string

//go:embed sql/cockroach_jobs.sql
var sqlCockroachJobs string

//go:embed sql/cockroach_contention.sql
var sqlCockroachContention string

//go:embed sql/cockroach_contention_attribution.sql
var sqlCockroachContentionAttribution string

//go:embed sql/cockroach_contention_attribution_legacy.sql
var sqlCockroachContentionAttributionLegacy string

type cockroachCollector struct{}

func (cockroachCollector) Name() string                          { return "cockroachdb" }
func (cockroachCollector) Kind() Kind                            { return KindGauge }
func (cockroachCollector) Available(caps conn.Capabilities) bool { return caps.IsCockroachDB() }

type cockroachLiveRow struct {
	QueryID        string  `db:"query_id"`
	UserName       string  `db:"user_name"`
	AppName        string  `db:"app_name"`
	Query          string  `db:"query"`
	AgeS           float64 `db:"age_s"`
	Distributed    bool    `db:"distributed"`
	FullScan       bool    `db:"full_scan"`
	Phase          string  `db:"phase"`
	IsolationLevel string  `db:"isolation_level"`
	Retries        int64   `db:"retries"`
	AutoRetries    int64   `db:"auto_retries"`
}

type cockroachInsightRow struct {
	Kind                 string     `db:"kind"`
	Fingerprint          string     `db:"fingerprint"`
	Problem              string     `db:"problem"`
	Causes               []string   `db:"causes"`
	Query                string     `db:"query"`
	Status               string     `db:"status"`
	StartedAt            *time.Time `db:"started_at"`
	EndedAt              *time.Time `db:"ended_at"`
	FullScan             bool       `db:"full_scan"`
	UserName             string     `db:"user_name"`
	AppName              string     `db:"app_name"`
	Retries              int64      `db:"retries"`
	LastRetryReason      string     `db:"last_retry_reason"`
	IndexRecommendations []string   `db:"index_recommendations"`
	RowsRead             int64      `db:"rows_read"`
	RowsWritten          int64      `db:"rows_written"`
	ContentionS          float64    `db:"contention_s"`
	ServiceLatencyMS     float64    `db:"service_latency_ms"`
	AdmissionWaitMS      float64    `db:"admission_wait_ms"`
	ErrorCode            string     `db:"error_code"`
}

type cockroachSample struct {
	Live                     []cockroachLiveRow
	LiveErr                  error
	Insights                 []cockroachInsightRow
	InsightsErr              error
	Jobs                     []cockroachJobRow
	JobsErr                  error
	HotRanges                []crdbhttp.HotRange
	HotRangesErr             error
	Contention               []cockroachContentionRow
	ContentionErr            error
	ContentionAttribution    []cockroachContentionAttributionRow
	ContentionAttributionErr error
}

type cockroachContentionRow struct {
	WaitingStmtFingerprint  string    `db:"waiting_stmt_fingerprint"`
	BlockingTxnFingerprint  string    `db:"blocking_txn_fingerprint"`
	BlockingStmtFingerprint string    `db:"blocking_stmt_fingerprint"`
	Database                string    `db:"database_name"`
	Schema                  string    `db:"schema_name"`
	Table                   string    `db:"table_name"`
	Index                   string    `db:"index_name"`
	Type                    string    `db:"contention_type"`
	Events                  int64     `db:"event_count"`
	TotalSeconds            float64   `db:"total_contention_seconds"`
	MaxSeconds              float64   `db:"max_contention_seconds"`
	LastSeen                time.Time `db:"last_seen"`
	TotalEvents             int64     `db:"total_events"`
	TotalSecondsAll         float64   `db:"total_contention_seconds_all"`
	MaxSecondsAll           float64   `db:"max_contention_seconds_all"`
	SerializationConflicts  int64     `db:"serialization_conflicts"`
}

type cockroachContentionAttributionRow struct {
	Kind                   string   `db:"attribution_kind"`
	Fingerprint            string   `db:"fingerprint"`
	TransactionFingerprint string   `db:"transaction_fingerprint"`
	Query                  string   `db:"query"`
	AppNames               []string `db:"app_names"`
}

type cockroachJobRow struct {
	JobID         string     `db:"job_id"`
	JobType       string     `db:"job_type"`
	State         string     `db:"state"`
	CreatedAt     time.Time  `db:"created_at"`
	FinishedAt    *time.Time `db:"finished_at"`
	Progress      float64    `db:"progress"`
	ProgressKnown bool       `db:"progress_known"`
	Operation     string     `db:"operation"`
	StatusMessage string     `db:"status_message"`
	Error         string     `db:"error"`
	LastUpdatedAt *time.Time `db:"last_updated_at"`
	HighWaterAt   *time.Time `db:"high_water_at"`
	TotalJobs     int64      `db:"total_jobs"`
}

func (cockroachCollector) Sample(ctx context.Context, t *conn.Target, caps conn.Capabilities) (any, error) {
	return (cockroachCollector{}).SampleWithOptions(ctx, t, caps, Options{})
}

func (cockroachCollector) SampleWithOptions(ctx context.Context, t *conn.Target, caps conn.Capabilities, opts Options) (any, error) {
	s := cockroachSample{}
	// The contention table and hot-range endpoint are both cluster fanouts. Run
	// them alongside the existing sequential SQL reads so the diagnostic does
	// not add their full latency to an already-busy cluster inspection.
	var wg sync.WaitGroup
	if caps.HasCRDBContention && caps.HasViewActivity {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Contention, s.ContentionErr = queryMany[cockroachContentionRow](ctx, t, sqlCockroachContention)
			if s.ContentionErr == nil && len(s.Contention) > 0 {
				s.ContentionAttribution, s.ContentionAttributionErr = queryCockroachContentionAttribution(ctx, t, caps, s.Contention)
			}
		}()
	}
	if opts.CockroachHTTP != nil && opts.CockroachHTTP.HasAdmin() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.HotRanges, s.HotRangesErr = opts.CockroachHTTP.HotRanges(ctx)
		}()
	}
	if caps.HasViewActivity {
		s.Live, s.LiveErr = queryMany[cockroachLiveRow](ctx, t, sqlCockroachLiveQueries)
		if caps.HasCRDBInsights {
			stmt, stmtErr := queryMany[cockroachInsightRow](ctx, t, sqlCockroachStatementInsights)
			txn, txnErr := queryMany[cockroachInsightRow](ctx, t, sqlCockroachTransactionInsights)
			s.Insights = append(stmt, txn...)
			s.InsightsErr = errors.Join(stmtErr, txnErr)
		}
	}
	if caps.HasCRDBJobs {
		s.Jobs, s.JobsErr = queryMany[cockroachJobRow](ctx, t, sqlCockroachJobs)
	}
	wg.Wait()
	return s, nil
}

func queryCockroachContentionAttribution(
	ctx context.Context, t *conn.Target, caps conn.Capabilities, rows []cockroachContentionRow,
) ([]cockroachContentionAttributionRow, error) {
	statementSet := make(map[string]struct{}, len(rows)*2)
	transactionSet := make(map[string]struct{}, len(rows))
	for _, r := range rows {
		for _, fp := range []string{r.WaitingStmtFingerprint, r.BlockingStmtFingerprint} {
			if fp = cleanCockroachFingerprint(fp); fp != "" {
				statementSet[fp] = struct{}{}
			}
		}
		if fp := cleanCockroachFingerprint(r.BlockingTxnFingerprint); fp != "" {
			transactionSet[fp] = struct{}{}
		}
	}
	statements := sortedCockroachFingerprints(statementSet)
	transactions := sortedCockroachFingerprints(transactionSet)
	if len(statements) == 0 && len(transactions) == 0 {
		return nil, nil
	}

	statementSQL, args := cockroachFingerprintPlaceholders(statements, 1)
	transactionSQL, transactionArgs := cockroachFingerprintPlaceholders(transactions, len(args)+1)
	args = append(args, transactionArgs...)
	template := sqlCockroachContentionAttributionLegacy
	if caps.HasCRDBStatementStore {
		template = sqlCockroachContentionAttribution
	}
	query := fmt.Sprintf(template, statementSQL, transactionSQL)
	return queryMany[cockroachContentionAttributionRow](ctx, t, query, args...)
}

func sortedCockroachFingerprints(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for fp := range set {
		out = append(out, fp)
	}
	sort.Strings(out)
	return out
}

func cockroachFingerprintPlaceholders(fingerprints []string, start int) (string, []any) {
	if len(fingerprints) == 0 {
		return "decode('', 'hex')", nil
	}
	items := make([]string, len(fingerprints))
	args := make([]any, len(fingerprints))
	for i, fp := range fingerprints {
		items[i] = fmt.Sprintf("decode($%d::STRING, 'hex')", start+i)
		args[i] = fp
	}
	return strings.Join(items, ", "), args
}

func (cockroachCollector) Assemble(c *model.Context, caps conn.Capabilities, s sampled, _ time.Duration, _ Options) {
	if !caps.IsCockroachDB() {
		return
	}
	d := &model.CockroachDB{}
	if !caps.HasViewActivity {
		reason := "role lacks VIEWACTIVITY — cluster workload is not visible"
		d.LiveQueries.Section = unavail(nil, reason)
		d.ExecutionInsights.Section = unavail(nil, reason)
		d.Contention.Section = unavail(nil, reason)
		c.Cockroach = d
	} else {
		assembleCockroachWorkload(d, caps, s)
		c.Cockroach = d
	}
	assembleCockroachOperations(c, caps, s)
}

func assembleCockroachWorkload(d *model.CockroachDB, caps conn.Capabilities, s sampled) {
	sm, ok := s.A.(cockroachSample)
	if s.Err != nil || !ok {
		err := s.Err
		if err == nil {
			err = errors.New("CockroachDB workload sample unavailable")
		}
		d.LiveQueries.Section = unavail(err, "")
		d.ExecutionInsights.Section = unavail(err, "")
		d.Contention.Section = unavail(err, "")
		return
	}
	if sm.LiveErr != nil {
		d.LiveQueries.Section = unavail(sm.LiveErr, "SHOW CLUSTER QUERIES unavailable")
	} else {
		d.LiveQueries.Section = model.Section{Exactness: model.ExactnessScraped}
		for _, r := range sm.Live {
			d.LiveQueries.Items = append(d.LiveQueries.Items, model.CockroachLiveQuery{
				QueryID: r.QueryID, User: r.UserName, AppName: r.AppName,
				Query: conn.ScrubRedactableText(r.Query), AgeSec: round2(max(0, r.AgeS)), Distributed: r.Distributed,
				FullScan: r.FullScan, Phase: r.Phase, Isolation: r.IsolationLevel,
				Retries: r.Retries, AutoRetries: r.AutoRetries,
			})
		}
	}
	if !caps.HasCRDBInsights {
		d.ExecutionInsights.Section = unavail(nil, "CockroachDB persisted execution insights are not available on this version")
	} else if sm.InsightsErr != nil {
		d.ExecutionInsights.Section = unavail(sm.InsightsErr, "CockroachDB execution insights read failed")
	} else {
		d.ExecutionInsights.Section = model.Section{Exactness: model.ExactnessCumulative}
		for _, r := range sm.Insights {
			d.ExecutionInsights.Items = append(d.ExecutionInsights.Items, model.CockroachInsight{
				Kind: r.Kind, Fingerprint: r.Fingerprint, Problem: r.Problem, Causes: r.Causes,
				Query: conn.ScrubRedactableText(r.Query), Status: r.Status, StartedAt: r.StartedAt, EndedAt: r.EndedAt,
				FullScan: r.FullScan, User: r.UserName, AppName: r.AppName, Retries: r.Retries,
				LastRetryReason: r.LastRetryReason, IndexRecommendations: r.IndexRecommendations,
				RowsRead: r.RowsRead, RowsWritten: r.RowsWritten, ContentionSec: round2(r.ContentionS),
				ServiceLatencyMS: round2(r.ServiceLatencyMS), AdmissionWaitMS: round2(r.AdmissionWaitMS), ErrorCode: r.ErrorCode,
			})
		}
	}
	assembleCockroachContention(d, caps, sm)
}

func assembleCockroachContention(d *model.CockroachDB, caps conn.Capabilities, sm cockroachSample) {
	if !caps.HasCRDBContention {
		d.Contention.Section = unavail(nil, "crdb_internal.transaction_contention_events is not available on this version")
		return
	}
	if sm.ContentionErr != nil {
		d.Contention.Section = unavail(sm.ContentionErr, "CockroachDB contention events read failed")
		return
	}
	d.Contention.Section = model.Section{Exactness: model.ExactnessScraped}
	d.Contention.WindowMinutes = 60
	d.Contention.Bounded = true
	if len(sm.Contention) > 0 {
		d.Contention.TotalEvents = sm.Contention[0].TotalEvents
		d.Contention.TotalWaitMS = round2(sm.Contention[0].TotalSecondsAll * 1000)
		d.Contention.MaxWaitMS = round2(sm.Contention[0].MaxSecondsAll * 1000)
		d.Contention.SerializationConflicts = sm.Contention[0].SerializationConflicts
	}
	for _, r := range sm.Contention {
		h := model.CockroachContentionHotspot{
			Database: r.Database, Schema: r.Schema, Table: r.Table, Index: r.Index, Type: r.Type,
			WaitingStatementFingerprint:  r.WaitingStmtFingerprint,
			BlockingTxnFingerprint:       cleanCockroachFingerprint(r.BlockingTxnFingerprint),
			BlockingStatementFingerprint: cleanCockroachFingerprint(r.BlockingStmtFingerprint),
			Events:                       r.Events, TotalWaitMS: round2(r.TotalSeconds * 1000), MaxWaitMS: round2(r.MaxSeconds * 1000), LastSeen: r.LastSeen.UTC(),
		}
		h.WaiterResolution = cockroachResolution(h.WaitingStatementFingerprint, sm.ContentionAttributionErr)
		h.BlockerResolution = cockroachResolution(h.BlockingTxnFingerprint, sm.ContentionAttributionErr)
		d.Contention.Hotspots = append(d.Contention.Hotspots, h)
	}
	applyCockroachContentionAttribution(d, sm.ContentionAttribution)
}

func cockroachResolution(fingerprint string, attributionErr error) string {
	if fingerprint == "" {
		return model.CockroachContentionNotResolved
	}
	if attributionErr != nil {
		return model.CockroachContentionStatsUnavailable
	}
	return model.CockroachContentionNotFound
}

type cockroachAttribution struct {
	query string
	apps  []string
}

func applyCockroachContentionAttribution(d *model.CockroachDB, rows []cockroachContentionAttributionRow) {
	statements := make(map[string]cockroachAttribution)
	transactions := make(map[string][]cockroachAttribution)
	for _, r := range rows {
		a := cockroachAttribution{query: conn.ScrubRedactableText(r.Query), apps: sortedStrings(r.AppNames)}
		if r.Kind == "transaction" {
			transactions[r.TransactionFingerprint] = append(transactions[r.TransactionFingerprint], a)
			continue
		}
		statements[r.Fingerprint] = a
	}
	for i := range d.Contention.Hotspots {
		h := &d.Contention.Hotspots[i]
		if a, ok := statements[h.WaitingStatementFingerprint]; ok && a.query != "" {
			h.WaitingQuery = a.query
			h.WaitingApplications = a.apps
			h.WaiterResolution = model.CockroachContentionResolved
		}
		if a, ok := statements[h.BlockingStatementFingerprint]; ok && a.query != "" {
			h.BlockingQuery = a.query
			h.BlockingApplications = a.apps
			h.BlockerResolution = model.CockroachContentionResolved
		}
		for _, a := range transactions[h.BlockingTxnFingerprint] {
			if a.query != "" {
				h.BlockingQueries = appendUniqueString(h.BlockingQueries, a.query)
			}
			h.BlockingApplications = appendUniqueStrings(h.BlockingApplications, a.apps...)
		}
		if len(h.BlockingQueries) > 0 {
			h.BlockerResolution = model.CockroachContentionResolved
		}
	}
}

func sortedStrings(items []string) []string {
	out := appendUniqueStrings(nil, items...)
	sort.Strings(out)
	return out
}

func appendUniqueStrings(dst []string, items ...string) []string {
	for _, item := range items {
		if item == "" {
			continue
		}
		found := false
		for _, old := range dst {
			if old == item {
				found = true
				break
			}
		}
		if !found {
			dst = append(dst, item)
		}
	}
	return dst
}

func appendUniqueString(dst []string, item string) []string {
	return appendUniqueStrings(dst, item)
}

func cleanCockroachFingerprint(s string) string {
	if s == "0000000000000000" {
		return ""
	}
	return s
}

func enrichCockroachContention(c *model.Context) {
	if c.Cockroach == nil || len(c.Cockroach.Contention.Hotspots) == 0 {
		return
	}
	queries := map[string]cockroachAttribution{}
	if c.Queries != nil {
		for _, q := range c.Queries.Top {
			if q.Fingerprint != "" && q.Query != "" {
				queries[q.Fingerprint] = cockroachAttribution{query: q.Query, apps: []string{q.AppName}}
			}
		}
	}
	for _, in := range c.Cockroach.ExecutionInsights.Items {
		if in.Fingerprint != "" && in.Query != "" {
			queries[in.Fingerprint] = cockroachAttribution{query: in.Query, apps: []string{in.AppName}}
		}
	}
	for i := range c.Cockroach.Contention.Hotspots {
		h := &c.Cockroach.Contention.Hotspots[i]
		if a, ok := queries[h.WaitingStatementFingerprint]; ok && h.WaitingQuery == "" {
			h.WaitingQuery = a.query
			h.WaitingApplications = appendUniqueStrings(h.WaitingApplications, a.apps...)
			h.WaiterResolution = model.CockroachContentionResolved
		}
		if a, ok := queries[h.BlockingStatementFingerprint]; ok && h.BlockingQuery == "" {
			h.BlockingQuery = a.query
			h.BlockingApplications = appendUniqueStrings(h.BlockingApplications, a.apps...)
			h.BlockerResolution = model.CockroachContentionResolved
		}
	}
}

func assembleCockroachOperations(c *model.Context, caps conn.Capabilities, s sampled) {
	if c.Health == nil {
		c.Health = &model.Health{Section: unavail(nil, "CockroachDB health unavailable")}
	}
	if c.Health.Cockroach == nil {
		c.Health.Cockroach = &model.CockroachHealth{}
	}
	h := c.Health.Cockroach
	defer assembleCockroachDistribution(h)
	sm, ok := s.A.(cockroachSample)
	if !ok {
		h.Jobs = unavail(s.Err, "CockroachDB jobs unavailable")
		h.HotRanges = unavail(s.Err, "CockroachDB hot ranges unavailable")
		return
	}
	if !caps.HasCRDBJobs {
		h.Jobs = unavail(nil, "information_schema.crdb_jobs_with_progress is not available on this version")
	} else if sm.JobsErr != nil {
		h.Jobs = unavail(sm.JobsErr, "CockroachDB jobs read failed")
	} else {
		h.Jobs = model.Section{Exactness: model.ExactnessScraped}
		if len(sm.Jobs) > 0 {
			h.JobsTotal = int(sm.Jobs[0].TotalJobs)
		}
		if h.JobsTotal == 0 {
			h.JobsTotal = len(sm.Jobs)
		}
		h.JobsBounded = h.JobsTotal > len(sm.Jobs)
		for _, r := range sm.Jobs {
			h.JobItems = append(h.JobItems, model.CockroachJobHealth{
				JobID: r.JobID, Type: r.JobType, State: r.State, CreatedAt: r.CreatedAt.UTC(),
				FinishedAt: utcTime(r.FinishedAt), Progress: round4(r.Progress), ProgressKnown: r.ProgressKnown,
				Operation: scrubCockroachJobOperation(r.Operation, 500), StatusMessage: scrubCockroachJobMessage(r.StatusMessage, 240),
				Error: scrubCockroachJobMessage(r.Error, 240), LastUpdatedAt: utcTime(r.LastUpdatedAt), HighWaterAt: utcTime(r.HighWaterAt),
			})
		}
	}
	if sm.HotRangesErr != nil && len(sm.HotRanges) == 0 {
		h.HotRanges = unavail(sm.HotRangesErr, "CockroachDB hot-ranges API unavailable")
	} else if len(sm.HotRanges) == 0 {
		h.HotRanges = unavail(nil, "configure --crdb-admin-url for physical hot ranges")
	} else {
		h.HotRanges = sourceSection(model.ExactnessScraped, sm.HotRangesErr)
		h.Hot = hottestRanges(sm.HotRanges, 10)
	}
}

func scrubCockroachJobOperation(s string, limit int) string {
	return trimCockroachJobText(conn.ScrubRedactableText(s), limit)
}

func scrubCockroachJobMessage(s string, limit int) string {
	return trimCockroachJobText(conn.ScrubDiagnosticText(s), limit)
}

func trimCockroachJobText(s string, limit int) string {
	s = strings.TrimSpace(s)
	if strings.Trim(s, " ?….,'\"") == "" {
		return ""
	}
	runes := []rune(s)
	if limit > 0 && len(runes) > limit {
		return strings.TrimSpace(string(runes[:limit])) + "…"
	}
	return s
}

func hottestRanges(rows []crdbhttp.HotRange, limit int) []model.CockroachHotRange {
	// One physical range may appear once per replica. Keep the replica reporting
	// the highest recent CPU so the cluster list has stable range identities.
	byRange := make(map[int64]crdbhttp.HotRange, len(rows))
	for _, r := range rows {
		if old, ok := byRange[r.RangeID]; !ok || r.CPUTimePerSecond > old.CPUTimePerSecond {
			byRange[r.RangeID] = r
		}
	}
	items := make([]model.CockroachHotRange, 0, len(byRange))
	for _, r := range byRange {
		items = append(items, model.CockroachHotRange{
			RangeID: r.RangeID, NodeID: r.NodeID, StoreID: r.StoreID, LeaseholderNodeID: r.LeaseholderNodeID,
			QPS: round2(r.QPS), CPUCores: round4(r.CPUTimePerSecond / 1e9),
			ReadsPerSec: round2(r.ReadsPerSecond), WritesPerSec: round2(r.WritesPerSecond),
			Databases: r.Databases, Schema: r.SchemaName, Tables: r.Tables, Indexes: r.Indexes,
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].CPUCores > items[j].CPUCores })
	if len(items) > limit {
		items = items[:limit]
	}
	return items
}

func utcTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}
