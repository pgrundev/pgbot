package collect

import (
	"context"
	_ "embed"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/model"
)

// LockEdge is one blocked→holder observation from one slow-plane snapshot:
// who waited, who held, and the holder's transaction context. Query text is
// RAW here and scrubbed by BuildWaitStudy.
type LockEdge struct {
	VictimPID      int
	HolderPID      int
	WaitEvent      string
	BlockedWaitS   float64
	HolderState    string
	HolderXactAgeS float64
	HolderQuery    string
	HolderUser     string
	HolderApp      string
	VictimQuery    string
}

// LockSnapshot is one 1 Hz look at the lock graph.
type LockSnapshot struct {
	Edges []LockEdge
}

// WaitStudyInput is everything the two sampling planes produced, plus the
// context needed to label the result honestly.
type WaitStudyInput struct {
	Fast          ashResult
	Snapshots     []LockSnapshot
	SnapshotFails int
	Hz            int
	HasPgMonitor  bool
	RawQueryText  bool
	FocusPID      int // 0 = every backend
}

// Blocker evidence thresholds: a holder seen in sustainedObs snapshots is
// named outright; corroboratedObs suffice when the victim's own fast-plane
// samples are majority Lock. Anything less is transient — reported, never
// blamed.
const (
	sustainedObs       = 3
	corroboratedObs    = 2
	victimLockShareBar = 0.5
)

// BuildWaitStudy turns raw samples and lock snapshots into the waits report.
// Pure and deterministic: same input, same output, ties broken by PID.
func BuildWaitStudy(in WaitStudyInput) *model.WaitStudy {
	scrub := func(s string) string {
		if in.RawQueryText {
			return s
		}
		return conn.ScrubQueryText(s)
	}

	samples := in.Fast.samples
	if in.FocusPID > 0 {
		var kept []WaitSample
		for _, s := range samples {
			if int(s.PID) == in.FocusPID {
				kept = append(kept, s)
			}
		}
		samples = kept
	}

	// Per-query sample text for the profile, from the samples themselves.
	texts := map[int64]string{}
	for _, s := range samples {
		if s.QueryID != nil && *s.QueryID != 0 && s.QueryText != "" {
			if _, ok := texts[*s.QueryID]; !ok {
				texts[*s.QueryID] = scrub(s.QueryText)
			}
		}
	}

	fast := in.Fast
	fast.samples = samples
	study := &model.WaitStudy{
		SchemaVersion:     model.WaitsSchemaVersion,
		Exactness:         string(model.ExactnessSampled),
		WindowSeconds:     round2(in.Fast.span.Seconds()),
		Hz:                in.Hz,
		Polls:             in.Fast.attempts,
		PollFailures:      in.Fast.failures,
		LockSnapshots:     len(in.Snapshots),
		LockSnapshotFails: in.SnapshotFails,
		Profile:           profileFrom(fast, texts),
	}
	if successful := in.Fast.attempts - in.Fast.failures; successful > 0 {
		study.AAS = round2(float64(len(samples)) / float64(successful))
	}
	study.Thin = study.Profile == nil || study.Profile.Thin() || !study.Profile.Available
	if !in.HasPgMonitor {
		study.Partial = "role lacks pg_monitor — only this role's own sessions were visible; the study covers a fraction of server activity"
	}

	study.Sessions = sessionRollup(samples, scrub)
	study.Blockers, study.Transient = blockerEvidence(in, samples, scrub)
	return study
}

// sessionRollup aggregates samples per backend PID.
func sessionRollup(samples []WaitSample, scrub func(string) string) []model.SessionWaits {
	if len(samples) == 0 {
		return nil
	}
	type agg struct {
		s     model.SessionWaits
		types map[string]int
		evs   map[string]int
	}
	byPID := map[int32]*agg{}
	for _, s := range samples {
		a := byPID[s.PID]
		if a == nil {
			a = &agg{types: map[string]int{}, evs: map[string]int{}}
			a.s.PID = int(s.PID)
			byPID[s.PID] = a
		}
		a.s.Count++
		if s.Usename != "" {
			a.s.User = s.Usename
		}
		if s.Datname != "" {
			a.s.DB = s.Datname
		}
		if s.AppName != "" {
			a.s.App = s.AppName
		}
		if s.QueryText != "" {
			a.s.SampleText = scrub(s.QueryText)
		}
		typ := "CPU"
		if s.WaitEventType != nil && *s.WaitEventType != "" {
			typ = *s.WaitEventType
		}
		a.types[typ]++
		if s.WaitEvent != nil && *s.WaitEvent != "" {
			a.evs[typ+":"+*s.WaitEvent]++
		}
	}
	total := len(samples)
	out := make([]model.SessionWaits, 0, len(byPID))
	for _, a := range byPID {
		a.s.Share = round2(float64(a.s.Count) / float64(total))
		a.s.TopType = topKey(a.types)
		a.s.TopEvent = topKey(a.evs)
		out = append(out, a.s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].PID < out[j].PID
	})
	return out
}

// blockerEvidence groups lock-snapshot edges by holder and applies the
// evidence gate. Victim lock shares come from the fast plane: corroboration,
// not narrative.
func blockerEvidence(in WaitStudyInput, samples []WaitSample, scrub func(string) string) (sustained, transient []model.Blocker) {
	// Victim PID → share of its samples in Lock class.
	lockShare := map[int]float64{}
	perPID, perPIDLock := map[int]int{}, map[int]int{}
	for _, s := range samples {
		pid := int(s.PID)
		perPID[pid]++
		if s.WaitEventType != nil && *s.WaitEventType == "Lock" {
			perPIDLock[pid]++
		}
	}
	for pid, n := range perPID {
		lockShare[pid] = float64(perPIDLock[pid]) / float64(n)
	}

	type holderAgg struct {
		b       model.Blocker
		victims map[int]*model.BlockedVictim
		seenIn  map[int]bool // snapshot index → seen
	}
	holders := map[int]*holderAgg{}
	for i, snap := range in.Snapshots {
		for _, e := range snap.Edges {
			if in.FocusPID > 0 && e.VictimPID != in.FocusPID {
				continue
			}
			h := holders[e.HolderPID]
			if h == nil {
				h = &holderAgg{victims: map[int]*model.BlockedVictim{}, seenIn: map[int]bool{}}
				h.b.HolderPID = e.HolderPID
				holders[e.HolderPID] = h
			}
			h.seenIn[i] = true
			if e.HolderXactAgeS >= h.b.HolderXactAgeS {
				h.b.HolderXactAgeS = round2(e.HolderXactAgeS)
				h.b.HolderState = e.HolderState
				h.b.HolderQuery = scrub(e.HolderQuery)
				h.b.HolderUser = e.HolderUser
				h.b.HolderApp = e.HolderApp
			}
			v := h.victims[e.VictimPID]
			if v == nil {
				v = &model.BlockedVictim{PID: e.VictimPID}
				h.victims[e.VictimPID] = v
			}
			if e.BlockedWaitS > v.MaxWaitS {
				v.MaxWaitS = round2(e.BlockedWaitS)
			}
			if e.WaitEvent != "" {
				v.WaitEvent = e.WaitEvent
			}
			if e.VictimQuery != "" {
				v.Query = scrub(e.VictimQuery)
			}
		}
	}

	for _, h := range holders {
		h.b.Observations = len(h.seenIn)
		for _, v := range h.victims {
			h.b.Victims = append(h.b.Victims, *v)
		}
		sort.Slice(h.b.Victims, func(i, j int) bool { return h.b.Victims[i].PID < h.b.Victims[j].PID })

		h.b.Sustained = h.b.Observations >= sustainedObs
		if !h.b.Sustained && h.b.Observations >= corroboratedObs {
			for _, v := range h.b.Victims {
				if lockShare[v.PID] >= victimLockShareBar {
					h.b.Sustained = true
					break
				}
			}
		}
		if h.b.Sustained {
			sustained = append(sustained, h.b)
		} else {
			transient = append(transient, h.b)
		}
	}
	byObsThenPID := func(s []model.Blocker) {
		sort.Slice(s, func(i, j int) bool {
			if s[i].Observations != s[j].Observations {
				return s[i].Observations > s[j].Observations
			}
			return s[i].HolderPID < s[j].HolderPID
		})
	}
	byObsThenPID(sustained)
	byObsThenPID(transient)
	return sustained, transient
}

//go:embed sql/waitlocks.sql
var sqlWaitLocks string

// lockEdgeRow mirrors sql/waitlocks.sql.
type lockEdgeRow struct {
	BlockedPID     int32   `db:"blocked_pid"`
	HolderPID      int32   `db:"holder_pid"`
	WaitEvent      string  `db:"wait_event"`
	BlockedWaitS   float64 `db:"blocked_wait_s"`
	VictimQuery    string  `db:"victim_query"`
	HolderState    string  `db:"holder_state"`
	HolderXactAgeS float64 `db:"holder_xact_age_s"`
	HolderQuery    string  `db:"holder_query"`
	HolderUser     string  `db:"holder_user"`
	HolderApp      string  `db:"holder_app"`
}

// WaitStudyOptions bounds one study run.
type WaitStudyOptions struct {
	Hz           int           // fast-plane rate (clamped by the CLI)
	Window       time.Duration // total sampling window
	FocusPID     int
	RawQueryText bool
}

// RunWaitStudy runs both sampling planes for the window and builds the report.
// Fast plane: pg_stat_activity at Hz (extended columns). Slow plane: the lock
// graph at 1 Hz — pg_blocking_pids walks the lock tables, so it gets its own
// budget and a failed snapshot is dropped, never queued behind a lock storm.
// Ctrl+C mid-window reports what was gathered (coverage says how much).
func RunWaitStudy(ctx context.Context, t *conn.Target, caps conn.Capabilities, o WaitStudyOptions) *model.WaitStudy {
	var snaps []LockSnapshot
	snapFails := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		sql := t.ExcludeSelf(sqlWaitLocks)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		deadline := time.Now().Add(o.Window)
		snapshot := func() {
			pctx, cancel := context.WithTimeout(ctx, ashPollBudget)
			defer cancel()
			rows, err := t.Pool.Query(pctx, sql)
			if err != nil {
				snapFails++
				return
			}
			got, err := pgx.CollectRows(rows, pgx.RowToStructByNameLax[lockEdgeRow])
			if err != nil {
				snapFails++
				return
			}
			snap := LockSnapshot{}
			for _, r := range got {
				snap.Edges = append(snap.Edges, LockEdge{
					VictimPID: int(r.BlockedPID), HolderPID: int(r.HolderPID),
					WaitEvent: r.WaitEvent, BlockedWaitS: r.BlockedWaitS,
					HolderState: r.HolderState, HolderXactAgeS: r.HolderXactAgeS,
					HolderQuery: r.HolderQuery, HolderUser: r.HolderUser,
					HolderApp: r.HolderApp, VictimQuery: r.VictimQuery,
				})
			}
			snaps = append(snaps, snap)
		}
		snapshot()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				if now.After(deadline) {
					return
				}
				snapshot()
			}
		}
	}()

	fast := sampleWaitsOpt(ctx, t, caps, o.Hz, o.Window, true)
	<-done

	return BuildWaitStudy(WaitStudyInput{
		Fast:          fast,
		Snapshots:     snaps,
		SnapshotFails: snapFails,
		Hz:            o.Hz,
		HasPgMonitor:  caps.HasPgMonitor,
		RawQueryText:  o.RawQueryText,
		FocusPID:      o.FocusPID,
	})
}
