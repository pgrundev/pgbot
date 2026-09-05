package why

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/pgrundev/pgbot/internal/model"
)

// Sample is one stored snapshot: its collection time and decoded context.
// (The store's Snapshot maps 1:1; the engine takes its own type so it never
// touches the database.)
type Sample struct {
	At time.Time
	C  *model.Context
}

// Options tunes an analysis.
type Options struct {
	MaxChains int // cap on reported chains; 0 = default 5
}

// Hop is one link of a chain, with the numbers that justify it.
type Hop struct {
	Role   string    `json:"role"` // mechanism | antecedent
	Text   string    `json:"text"`
	At     time.Time `json:"at,omitempty"`
	Before float64   `json:"before,omitempty"`
	After  float64   `json:"after,omitempty"`
}

// Chain is a symptom and the causal hops behind it, worst first.
type Chain struct {
	Symptom    Hop     `json:"symptom"`
	Hops       []Hop   `json:"hops"`
	Confidence float64 `json:"confidence"`
	impact     float64 // sort key: how much this symptom matters
}

// Report is what `pgbot why` renders. Its JSON shape is versioned separately
// from the Context contract.
type Report struct {
	SchemaVersion string    `json:"why_schema_version"`
	Database      string    `json:"database"`
	WindowStart   time.Time `json:"window_start"`
	WindowEnd     time.Time `json:"window_end"`
	Snapshots     int       `json:"snapshots"`
	// Scope and totals, so consumers can say "analyzed X, found Y, showing Z"
	// instead of presenting a bare list as if it were everything.
	AnalyzedQueries  int      `json:"analyzed_queries"`
	AnalyzedTables   int      `json:"analyzed_tables"`
	RegressionsFound int      `json:"regressions_found"`
	Chains           []Chain  `json:"chains"`
	Notes            []string `json:"notes,omitempty"`
	// Live is the wait-study diagnosis when `why --duration` sampled the
	// database — additive (1.1.0); offline reports omit it.
	Live *LiveReport `json:"live,omitempty"`
}

const whySchemaVersion = "1.1.0"

// minSamples is the least history an onset can stand on.
const minSamples = 3

// growthAntecedentPct is the first-to-last table growth that counts as a
// planner-flip antecedent for a seq-scan surge.
const growthAntecedentPct = 0.10

// Analyze computes causal chains from snapshot history and persisted events.
// Deterministic: same samples, same report.
func Analyze(samples []Sample, events []model.Event, opts Options) Report {
	r := Report{SchemaVersion: whySchemaVersion}
	if n := len(samples); n > 0 {
		r.Database = samples[0].C.Server.Database
		r.WindowStart, r.WindowEnd = samples[0].At, samples[n-1].At
		r.Snapshots = n
	}
	if len(samples) < minSamples {
		r.Notes = append(r.Notes, fmt.Sprintf(
			"only %d snapshot(s) in the window — pgbot needs at least %d to tell a change from a baseline. Run `pgbot inspect` a few times as the workload runs (each run stores one), then come back.",
			len(samples), minSamples))
		return r
	}

	r.AnalyzedQueries = len(queryIDs(samples))
	r.AnalyzedTables = len(tableNames(samples))

	chains := querySlowdownChains(samples, events)
	r.RegressionsFound = len(chains)

	sort.SliceStable(chains, func(i, j int) bool { return chains[i].impact > chains[j].impact })
	max := opts.MaxChains
	if max <= 0 {
		max = 5
	}
	if len(chains) > max {
		chains = chains[:max]
	}
	r.Chains = chains
	return r
}

// tableNames returns every qualified table seen in the samples.
func tableNames(samples []Sample) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range samples {
		if s.C.Tables == nil {
			continue
		}
		for _, t := range s.C.Tables.Top {
			q := t.Schema + "." + t.Name
			if !seen[q] {
				seen[q] = true
				out = append(out, q)
			}
		}
	}
	return out
}

// querySlowdownChains is mechanism rule 1: a query's interval mean shifted up;
// the mechanism is a seq-scan surge on a table the query references, and the
// antecedents are what set the planner off — table growth past the threshold,
// or an index dropped shortly before.
func querySlowdownChains(samples []Sample, events []model.Event) []Chain {
	var chains []Chain
	for _, qid := range queryIDs(samples) {
		meanSeries, text, share := queryIntervalMeans(samples, qid)
		shift := detectShift(meanSeries, shiftCfg{MinRatio: 1.5})
		if shift == nil {
			continue
		}
		ratio := 0.0
		if shift.Before > 0 {
			ratio = shift.After / shift.Before
		}
		ch := Chain{
			Symptom: Hop{
				Role: "symptom",
				Text: fmt.Sprintf("query %d (%s) slowed %.1f× — mean %s → %s per call since %s",
					qid, compactQuery(text), ratio, ms(shift.Before), ms(shift.After), shift.At.Format("Jan 2 15:04")),
				At: shift.At, Before: shift.Before, After: shift.After,
			},
			impact: share * ratio,
		}
		conf := 0.35 // a lone unexplained slowdown is a possibility, not a diagnosis

		for _, tbl := range referencedTables(samples, text) {
			rate := tableSeqScanRates(samples, tbl)
			mech := detectShift(rate, shiftCfg{MinRatio: 1.5})
			if mech == nil || mech.At.After(shift.At) {
				continue // no surge, or it started after the symptom — not a cause
			}
			ch.Hops = append(ch.Hops, Hop{
				Role: "mechanism",
				Text: fmt.Sprintf("because seq scans on %s surged %s → %s per second at %s",
					tbl, rateStr(mech.Before), rateStr(mech.After), mech.At.Format("Jan 2 15:04")),
				At: mech.At, Before: mech.Before, After: mech.After,
			})
			conf = 0.5
			if !mech.At.After(shift.At) && shift.At.Sub(mech.At) <= interSampleGap(samples)*2 {
				conf += 0.05 // tight alignment
			}

			if pct := tableGrowthPct(samples, tbl); pct >= growthAntecedentPct {
				ch.Hops = append(ch.Hops, Hop{
					Role: "antecedent",
					Text: fmt.Sprintf("after %s grew %.0f%% over the window (%s → %s) — enough to flip the planner off an index path",
						tbl, pct*100, bytesStr(tableBytesAt(samples, tbl, 0)), bytesStr(tableBytesAt(samples, tbl, -1))),
				})
				conf += 0.15
			}
			for _, ev := range events {
				if ev.Kind != "schema.index_dropped" || !indexServesTable(ev.Object, tbl) {
					continue
				}
				at := eventTime(ev)
				if at.IsZero() || at.After(mech.At) {
					continue
				}
				ch.Hops = append(ch.Hops, Hop{
					Role: "antecedent",
					Text: fmt.Sprintf("after index %s was dropped (observed by %s) — its name suggests it served %s",
						ev.Object, at.Format("Jan 2 15:04"), tbl),
					At: at,
				})
				conf += 0.15
			}
			if ratio >= 2 && (mech.Before == 0 || mech.After/max64(mech.Before, 1e-9) >= 3) {
				conf += 0.1 // both shifts are large, not borderline
			}
		}
		if conf > 0.9 {
			conf = 0.9
		}
		ch.Confidence = conf
		chains = append(chains, ch)
	}
	return chains
}

// --- series builders -------------------------------------------------------

// queryIDs returns every queryid seen in the samples, in first-seen order.
func queryIDs(samples []Sample) []int64 {
	seen := map[int64]bool{}
	var out []int64
	for _, s := range samples {
		if s.C.Queries == nil {
			continue
		}
		for _, q := range s.C.Queries.Top {
			if !seen[q.QueryID] {
				seen[q.QueryID] = true
				out = append(out, q.QueryID)
			}
		}
	}
	return out
}

// queryIntervalMeans builds the per-interval mean series for one query:
// (ΔTotalMS / ΔCalls) between adjacent snapshots, stamped at the later time.
// This is the honest slowdown signal — pg_stat_statements' own mean is a
// lifetime average that dilutes a fresh regression. Also returns the latest
// query text and the query's share of total DB time (the impact sort key).
func queryIntervalMeans(samples []Sample, qid int64) (series []Point, text string, share float64) {
	var prev *model.QueryStat
	for _, s := range samples {
		q := findQuery(s.C, qid)
		if q == nil {
			prev = nil
			continue
		}
		text = q.Query
		if s.C.Queries.TotalExecMS > 0 {
			share = q.TotalMS / s.C.Queries.TotalExecMS
		}
		if prev != nil {
			dc := q.Calls - prev.Calls
			dms := q.TotalMS - prev.TotalMS
			if dc > 0 && dms >= 0 {
				series = append(series, Point{At: s.At, Val: dms / float64(dc)})
			}
		}
		prev = q
	}
	return series, text, share
}

func findQuery(c *model.Context, qid int64) *model.QueryStat {
	if c.Queries == nil {
		return nil
	}
	for i := range c.Queries.Top {
		if c.Queries.Top[i].QueryID == qid {
			return &c.Queries.Top[i]
		}
	}
	return nil
}

// tableSeqScanRates builds the seq-scans-per-second series for one table.
// A negative delta is a stats reset: the point is skipped, never interpolated.
func tableSeqScanRates(samples []Sample, table string) []Point {
	var out []Point
	var prev *model.TableStat
	var prevAt time.Time
	for _, s := range samples {
		t := findTable(s.C, table)
		if t == nil {
			prev = nil
			continue
		}
		if prev != nil {
			d := t.SeqScans - prev.SeqScans
			dt := s.At.Sub(prevAt).Seconds()
			if d >= 0 && dt > 0 {
				out = append(out, Point{At: s.At, Val: float64(d) / dt})
			}
		}
		prev, prevAt = t, s.At
	}
	return out
}

func findTable(c *model.Context, qualified string) *model.TableStat {
	if c.Tables == nil {
		return nil
	}
	for i := range c.Tables.Top {
		t := &c.Tables.Top[i]
		if t.Schema+"."+t.Name == qualified {
			return t
		}
	}
	return nil
}

// referencedTables returns the qualified tables (from any snapshot) whose bare
// name appears as an identifier in the query text.
func referencedTables(samples []Sample, queryText string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range samples {
		if s.C.Tables == nil {
			continue
		}
		for _, t := range s.C.Tables.Top {
			q := t.Schema + "." + t.Name
			if seen[q] {
				continue
			}
			if referencesTable(queryText, t.Name) {
				seen[q] = true
				out = append(out, q)
			}
		}
	}
	sort.Strings(out)
	return out
}

// referencesTable reports whether the normalized query text mentions the table
// name as a whole identifier (word-bounded, case-insensitive).
func referencesTable(queryText, table string) bool {
	re := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(table) + `\b`)
	return re.MatchString(queryText)
}

// tableGrowthPct is first-to-last relative growth of the table's total bytes.
func tableGrowthPct(samples []Sample, table string) float64 {
	first, last := tableBytesAt(samples, table, 0), tableBytesAt(samples, table, -1)
	if first <= 0 {
		return 0
	}
	return float64(last-first) / float64(first)
}

// tableBytesAt returns the table's size in the first (idx 0) or last (idx -1)
// sample that carries it.
func tableBytesAt(samples []Sample, table string, idx int) int64 {
	if idx == -1 {
		for i := len(samples) - 1; i >= 0; i-- {
			if t := findTable(samples[i].C, table); t != nil {
				return t.TotalBytes
			}
		}
		return 0
	}
	for _, s := range samples {
		if t := findTable(s.C, table); t != nil {
			return t.TotalBytes
		}
	}
	return 0
}

// indexServesTable is the honest heuristic tying a dropped index to a table:
// the event's object is schema.indexname, and conventionally the index name
// embeds the table name. The hop text says "name suggests" for exactly this
// reason.
func indexServesTable(indexObject, qualifiedTable string) bool {
	_, table, ok := strings.Cut(qualifiedTable, ".")
	if !ok {
		table = qualifiedTable
	}
	return strings.Contains(strings.ToLower(indexObject), strings.ToLower(table))
}

// eventTime is the best timestamp an event offers (upper bound preferred: "it
// had happened by then").
func eventTime(ev model.Event) time.Time {
	if ev.OccurredBefore != nil {
		return *ev.OccurredBefore
	}
	if ev.OccurredAfter != nil {
		return *ev.OccurredAfter
	}
	return time.Time{}
}

// interSampleGap is the median gap between samples — the natural unit of
// "shortly before" for alignment scoring.
func interSampleGap(samples []Sample) time.Duration {
	if len(samples) < 2 {
		return time.Hour
	}
	gaps := make([]time.Duration, 0, len(samples)-1)
	for i := 1; i < len(samples); i++ {
		gaps = append(gaps, samples[i].At.Sub(samples[i-1].At))
	}
	sort.Slice(gaps, func(i, j int) bool { return gaps[i] < gaps[j] })
	return gaps[len(gaps)/2]
}

// --- formatting helpers ----------------------------------------------------

func compactQuery(q string) string {
	q = strings.Join(strings.Fields(q), " ")
	r := []rune(q)
	if len(r) > 60 {
		return string(r[:59]) + "…"
	}
	return q
}

// ms renders a millisecond value without erasing it: sub-millisecond means are
// real (a 9.7× slowdown can live entirely below 1ms) and must keep their
// significant digits, while big values stay terse.
func ms(v float64) string {
	switch {
	case v >= 1000:
		return fmt.Sprintf("%.1fs", v/1000)
	case v >= 10:
		return fmt.Sprintf("%.0fms", v)
	case v >= 1:
		return fmt.Sprintf("%.1fms", v)
	default:
		return fmt.Sprintf("%.2gms", v)
	}
}

// rateStr keeps significant digits on small per-second rates ("0.0042"), terse
// on large ones ("50").
func rateStr(v float64) string {
	if v < 1 {
		return fmt.Sprintf("%.2g", v)
	}
	return fmt.Sprintf("%.0f", v)
}

func bytesStr(b int64) string {
	const gib = 1 << 30
	const mib = 1 << 20
	switch {
	case b >= gib:
		return fmt.Sprintf("%.1f GiB", float64(b)/gib)
	case b >= mib:
		return fmt.Sprintf("%.0f MiB", float64(b)/mib)
	default:
		return fmt.Sprintf("%d B", b)
	}
}

func max64(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
