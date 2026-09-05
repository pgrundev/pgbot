package why

import (
	"strings"
	"testing"

	"github.com/pgrundev/pgbot/internal/model"
)

func study(aas float64, buckets ...model.WaitBucket) *model.WaitStudy {
	total := 0
	for _, b := range buckets {
		total += b.Count
	}
	return &model.WaitStudy{
		AAS: aas, Polls: 100, LockSnapshots: 10, Hz: 10, WindowSeconds: 10,
		Profile: &model.WaitProfile{Available: true, Samples: total, Buckets: buckets},
	}
}

func bucket(typ string, count int, total int) model.WaitBucket {
	return model.WaitBucket{Type: typ, Count: count, Share: float64(count) / float64(total)}
}

func sustainedBlocker() model.Blocker {
	return model.Blocker{HolderPID: 8172, HolderState: "idle in transaction",
		HolderXactAgeS: 43, Observations: 9, Sustained: true,
		Victims: []model.BlockedVictim{{PID: 18442}}}
}

// The rules are FIRST-MATCH deterministic; the gates outrank every diagnosis.
func TestClassifyLiveGatesOutrankCauses(t *testing.T) {
	// Thin study with a perfect blocker story must still refuse to conclude.
	s := study(3, bucket("Lock", 50, 50))
	s.Thin = true
	s.Blockers = []model.Blocker{sustainedBlocker()}
	if r := ClassifyLive(s, nil); r.Cause != "insufficient_evidence" {
		t.Errorf("thin must gate everything: %+v", r)
	}
	// Partial visibility gates too.
	s = study(3, bucket("Lock", 50, 50))
	s.Partial = "role lacks pg_monitor"
	if r := ClassifyLive(s, nil); r.Cause != "insufficient_evidence" {
		t.Errorf("partial must gate: %+v", r)
	}
	// Coverage under half the intended polls gates.
	s = study(3, bucket("Lock", 50, 50))
	s.PollFailures = 60
	if r := ClassifyLive(s, nil); r.Cause != "insufficient_evidence" {
		t.Errorf("poor coverage must gate: %+v", r)
	}
	// Negligible activity: high wait share of nearly no work is noise.
	s = study(0.2, bucket("Lock", 30, 30))
	s.Blockers = []model.Blocker{sustainedBlocker()}
	if r := ClassifyLive(s, nil); r.Cause != "not_significant" {
		t.Errorf("AAS below the floor must not diagnose: %+v", r)
	}
}

func TestClassifyLiveCauses(t *testing.T) {
	// Lock share + sustained blocker → contention, blocker named, index ruled out.
	s := study(3, bucket("Lock", 60, 100), bucket("CPU", 40, 100))
	s.Blockers = []model.Blocker{sustainedBlocker()}
	r := ClassifyLive(s, nil)
	if r.Cause != "lock_contention" || r.Confidence < 0.5 {
		t.Fatalf("contention expected: %+v", r)
	}
	joined := r.Headline + " " + strings.Join(r.Evidence, " ")
	for _, want := range []string{"8172", "43", "no evidence that a missing index"} {
		if !strings.Contains(joined, want) {
			t.Errorf("contention output missing %q: %s", want, joined)
		}
	}

	// Lock share WITHOUT a sustained blocker is a possibility, not a diagnosis.
	s = study(3, bucket("Lock", 60, 100), bucket("CPU", 40, 100))
	if r := ClassifyLive(s, nil); r.Cause != "lock_churn" || r.Confidence >= 0.5 {
		t.Errorf("no blocker evidence must stay below diagnosis confidence: %+v", r)
	}

	// IO-heavy: storage wait — and DataFileRead alone never recommends an index.
	s = study(3, bucket("IO", 70, 100), bucket("CPU", 30, 100))
	r = ClassifyLive(s, nil)
	if r.Cause != "storage_wait" {
		t.Fatalf("storage expected: %+v", r)
	}
	if strings.Contains(strings.ToLower(r.Headline+strings.Join(r.Evidence, " ")), "missing index") {
		t.Errorf("IO must not auto-claim a missing index: %+v", r)
	}
	if r.NextCheck != "" {
		t.Errorf("no dominant query → no next-check pointer: %+v", r)
	}
	// One query owning the IO earns the advise POINTER (not a recommendation).
	s.Profile.ByQuery = []model.QueryWaits{{QueryID: 7, Share: 0.6, IOShare: 0.9, SampleText: "SELECT …"}}
	if r := ClassifyLive(s, nil); !strings.Contains(r.NextCheck, "pgbot advise") {
		t.Errorf("dominant IO query should point at advise: %+v", r)
	}

	// Client waits are the application's, not Postgres's.
	s = study(3, bucket("Client", 80, 100), bucket("CPU", 20, 100))
	if r := ClassifyLive(s, nil); r.Cause != "client_wait" ||
		!strings.Contains(strings.Join(r.Evidence, " "), "not a PostgreSQL performance problem") {
		t.Errorf("client wait wording wrong: %+v", r)
	}

	// CPU-dominant: executing, not waiting.
	s = study(6, bucket("CPU", 80, 100), bucket("IO", 20, 100))
	if r := ClassifyLive(s, nil); r.Cause != "cpu_saturated" {
		t.Errorf("cpu expected: %+v", r)
	}

	// Nothing dominant → mixed, low confidence, no invented cause.
	s = study(3, bucket("CPU", 35, 100), bucket("IO", 35, 100), bucket("Lock", 30, 100))
	if r := ClassifyLive(s, nil); r.Cause != "mixed" || r.Confidence >= 0.5 {
		t.Errorf("mixed expected: %+v", r)
	}
}

// History corroboration adds a labeled note and confidence, never silently
// blending windows; a thin baseline is ignored.
func TestClassifyLiveHistory(t *testing.T) {
	s := study(3, bucket("Lock", 60, 100), bucket("CPU", 40, 100))
	s.Blockers = []model.Blocker{sustainedBlocker()}

	base := ClassifyLive(s, nil).Confidence
	hist := &HistShares{Shares: map[string]float64{"Lock": 0.075}, Samples: 5000, Desc: "previous 24h (local store)"}
	r := ClassifyLive(s, hist)
	if !strings.Contains(r.HistoryNote, "8×") || !strings.Contains(r.HistoryNote, "previous 24h") {
		t.Errorf("history note wrong: %q", r.HistoryNote)
	}
	if r.Confidence <= base {
		t.Errorf("corroborating history must add confidence: %v ≤ %v", r.Confidence, base)
	}

	thin := &HistShares{Shares: map[string]float64{"Lock": 0.075}, Samples: 10, Desc: "previous 24h"}
	if r := ClassifyLive(s, thin); r.HistoryNote != "" {
		t.Errorf("a thin baseline must be ignored: %q", r.HistoryNote)
	}
}

// The source label is always present — future pg_wait_sampling mode changes it.
func TestClassifyLiveSource(t *testing.T) {
	if r := ClassifyLive(study(3, bucket("CPU", 60, 60)), nil); r.Source != "pgbot-ash" {
		t.Errorf("source label missing: %+v", r)
	}
}
