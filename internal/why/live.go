package why

import (
	"fmt"
	"strings"

	"github.com/pgrundev/pgbot/internal/model"
)

// LiveReport is the diagnosis `pgbot why --duration` derives from one live
// wait study. Cause selection is FIRST-MATCH deterministic over named
// thresholds; the gates (insufficient evidence, negligible activity) outrank
// every diagnosis, because refusing to conclude beats a confident guess.
type LiveReport struct {
	Cause       string           `json:"cause"`
	Confidence  float64          `json:"confidence"`
	Headline    string           `json:"headline"`
	Evidence    []string         `json:"evidence,omitempty"`
	NextCheck   string           `json:"next_check,omitempty"`
	HistoryNote string           `json:"history_note,omitempty"`
	Source      string           `json:"source"` // "pgbot-ash"; pg_wait_sampling later
	Study       *model.WaitStudy `json:"study,omitempty"`
}

// ioVerdict turns the window's pg_stat_io reading into one evidence line and a
// storage-bound flag. Needs track_io_timing and enough reads for the mean to
// hold (ioVerdictMinReads); otherwise it says what is missing.
func ioVerdict(io *model.IOStats) (line string, storage bool) {
	if io == nil || io.Exactness != model.ExactnessSampled {
		return "", false
	}
	if !io.TrackIOTiming {
		return "pg_stat_io: track_io_timing is off, so per-read latency is unknown — turn it on (a reload) to tell device waits from cache misses.", false
	}
	if io.ReadLatencyMS == nil || io.ReadsInWindow < ioVerdictMinReads {
		return fmt.Sprintf("pg_stat_io: only %d physical reads in the window — too few to judge storage latency.", io.ReadsInWindow), false
	}
	lat := *io.ReadLatencyMS
	if lat >= ioVerdictStorageMS {
		return fmt.Sprintf("pg_stat_io: %d physical reads averaged %.2f ms each — the device (or volume latency floor), not the page cache, served them.", io.ReadsInWindow, lat), true
	}
	return fmt.Sprintf("pg_stat_io: %d physical reads averaged %.2f ms each — served from the kernel page cache; the cost is block volume, not device latency.", io.ReadsInWindow, lat), false
}

// HistShares is a baseline wait-class distribution from the local store's
// rollups — a separate window, compared by ratio, never blended into the live
// percentages.
type HistShares struct {
	Shares  map[string]float64 // class → share within the baseline window
	Samples int                // total baseline samples; thin baselines are ignored
	Desc    string             // e.g. "previous 24h (local store)"
}

// The rule thresholds, named so the tests pin them.
const (
	liveCoverageFloor  = 0.5 // successful polls / polls
	liveAASFloor       = 0.5
	liveLockShareBar   = 0.40
	liveIOShareBar     = 0.40
	ioVerdictMinReads  = 500 // reads in the window before a mean latency means anything
	ioVerdictStorageMS = 1.0 // ≥ 1 ms per read is a device, not the page cache
	liveClientShareBar = 0.50
	liveCPUShareBar    = 0.60
	histMinSamples     = 100
	histRatioBar       = 2.0
	ioQueryShareBar    = 0.3 // one query's share of all samples...
	ioQueryIOBar       = 0.5 // ...and of its own samples on IO, to earn the advise pointer
)

// ClassifyLive turns a wait study (and optional history baseline) into a
// cause. Shares quoted are always sampled; the only exact numbers are ages
// read from the server.
func ClassifyLive(s *model.WaitStudy, hist *HistShares) *LiveReport {
	r := &LiveReport{Source: "pgbot-ash", Study: s}

	// Gates first.
	coverage := 1.0
	if s != nil && s.Polls > 0 {
		coverage = float64(s.Polls-s.PollFailures) / float64(s.Polls)
	}
	switch {
	case s == nil || s.Profile == nil || !s.Profile.Available:
		r.Cause, r.Headline = "insufficient_evidence", "sampling failed — no conclusion."
		return r
	case s.Thin:
		r.Cause = "insufficient_evidence"
		r.Headline = fmt.Sprintf("too few samples (%d) to conclude — re-run with a longer --duration.", s.Profile.Samples)
		return r
	case s.Partial != "":
		r.Cause = "insufficient_evidence"
		r.Headline = "visibility was partial: " + s.Partial
		return r
	case coverage < liveCoverageFloor:
		r.Cause = "insufficient_evidence"
		r.Headline = fmt.Sprintf("sampling covered only %.0f%% of the window — no conclusion.", coverage*100)
		return r
	case s.AAS < liveAASFloor:
		r.Cause = "not_significant"
		r.Headline = fmt.Sprintf("average active sessions was %.1f — waits exist, but wait share of near-zero activity is noise, not a bottleneck.", s.AAS)
		return r
	}

	share := map[string]float64{}
	for _, b := range s.Profile.Buckets {
		share[b.Type] = b.Share
	}

	switch {
	case share["Lock"] >= liveLockShareBar && len(s.Blockers) > 0:
		b := s.Blockers[0]
		r.Cause = "lock_contention"
		r.Confidence = 0.7 + min(0.15, 0.03*float64(b.Observations))
		r.Headline = "transaction lock contention"
		r.Evidence = append(r.Evidence,
			fmt.Sprintf("%.0f%% of sampled time went to Lock waits (%d sessions averaged active).", share["Lock"]*100, int(s.AAS+0.5)))
		for _, v := range b.Victims {
			line := fmt.Sprintf("PID %d waited on Lock", v.PID)
			if v.WaitEvent != "" {
				line += ":" + v.WaitEvent
			}
			r.Evidence = append(r.Evidence, line+".")
		}
		r.Evidence = append(r.Evidence,
			fmt.Sprintf("Blocked by PID %d (%s) — its transaction has been open for %.0f seconds (exact), seen in %d lock snapshots.",
				b.HolderPID, b.HolderState, b.HolderXactAgeS, b.Observations),
			"There is no evidence that a missing index caused this.")
	case share["Lock"] >= liveLockShareBar:
		r.Cause = "lock_churn"
		r.Confidence = 0.4
		r.Headline = "possibly lock contention — no single blocker was observed long enough to name"
		r.Evidence = append(r.Evidence,
			fmt.Sprintf("%.0f%% of sampled time in Lock waits, but no holder persisted across lock snapshots.", share["Lock"]*100))
	case share["IO"] >= liveIOShareBar:
		r.Cause = "storage_wait"
		r.Confidence = 0.6
		r.Headline = "storage/WAL wait"
		r.Evidence = append(r.Evidence,
			fmt.Sprintf("%.0f%% of sampled time reading or writing data — IO wait alone does not identify a cause like an absent index.", share["IO"]*100))
		if line, storage := ioVerdict(s.IO); line != "" {
			r.Evidence = append(r.Evidence, line)
			if storage {
				r.Confidence = 0.7
				r.Headline = "storage latency — physical reads wait on the device"
				r.NextCheck = "reads are slow per block, not just many: cut blocks read first (top queries by shared_blks_read, indexes, bloat), then working set vs shared_buffers, then effective_io_concurrency / io_method / volume class"
			} else {
				r.NextCheck = "reads were served in microseconds (kernel page cache), so the IO time is volume, not device latency: find the query reading the most blocks — `pgbot queries`, then `pgbot advise`"
			}
		}
		for _, q := range s.Profile.ByQuery {
			if q.Share >= ioQueryShareBar && q.IOShare >= ioQueryIOBar {
				r.NextCheck = "one query dominates the IO samples — `pgbot advise` can check indexes with planner validation"
				break
			}
		}
	case share["Client"] >= liveClientShareBar:
		r.Cause = "client_wait"
		r.Confidence = 0.55
		r.Headline = "waiting on the client/application"
		r.Evidence = append(r.Evidence,
			fmt.Sprintf("%.0f%% of sampled time waiting for the application to send or receive — this is not a PostgreSQL performance problem.", share["Client"]*100))
	case share["CPU"] >= liveCPUShareBar:
		r.Cause = "cpu_saturated"
		r.Confidence = 0.6
		r.Headline = "saturated with active work"
		r.Evidence = append(r.Evidence,
			fmt.Sprintf("%.0f%% of samples on CPU with %.1f sessions averaged active — the server is executing, not waiting.", share["CPU"]*100, s.AAS))
	default:
		r.Cause = "mixed"
		r.Confidence = 0.35
		r.Headline = "no single dominant wait class"
		var tops []string
		for _, b := range s.Profile.Buckets {
			tops = append(tops, fmt.Sprintf("%s %.0f%%", b.Type, b.Share*100))
			if len(tops) == 3 {
				break
			}
		}
		r.Evidence = append(r.Evidence, "Top classes: "+strings.Join(tops, ", ")+" — sampled shares, no single cause claimed.")
	}

	// History corroboration: ratio against a labeled baseline window, only
	// when the baseline is substantial. Windows are compared, never blended.
	if hist != nil && hist.Samples >= histMinSamples && len(s.Profile.Buckets) > 0 {
		top := s.Profile.Buckets[0]
		if base := hist.Shares[top.Type]; base > 0 {
			ratio := top.Share / base
			if ratio >= histRatioBar {
				r.HistoryNote = fmt.Sprintf("%s waits %.0f× vs %s.", top.Type, ratio, hist.Desc)
				r.Confidence = min(0.95, r.Confidence+0.1)
			} else if ratio <= 1/histRatioBar {
				r.HistoryNote = fmt.Sprintf("%s waits %.0f× LOWER than %s — this window may not represent the incident.", top.Type, 1/ratio, hist.Desc)
			}
		}
	}
	return r
}

func min(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
