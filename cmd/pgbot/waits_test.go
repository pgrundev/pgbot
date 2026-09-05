package main

import (
	"strings"
	"testing"
	"time"

	"github.com/pgrundev/pgbot/internal/model"
)

func TestClampWaits(t *testing.T) {
	d, hz := clampWaits(10*time.Second, 10)
	if d != 10*time.Second || hz != 10 {
		t.Errorf("defaults must pass through: %v %v", d, hz)
	}
	if d, _ := clampWaits(0, 10); d != time.Second {
		t.Errorf("duration floor is 1s: %v", d)
	}
	if d, _ := clampWaits(time.Hour, 10); d != 5*time.Minute {
		t.Errorf("duration ceiling is 5m: %v", d)
	}
	if _, hz := clampWaits(time.Second, 0); hz != 1 {
		t.Errorf("hz floor is 1: %v", hz)
	}
	if _, hz := clampWaits(time.Second, 100); hz != 20 {
		t.Errorf("hz ceiling is 20 — sampling overhead stays bounded: %v", hz)
	}
}

func TestParseWaitsGroup(t *testing.T) {
	for _, ok := range []string{"event", "query", "session"} {
		if _, err := parseWaitsGroup(ok); err != nil {
			t.Errorf("%s must be valid: %v", ok, err)
		}
	}
	if _, err := parseWaitsGroup("bogus"); err == nil {
		t.Error("unknown group must be a usage error")
	}
}

// The conclusion is evidence-gated prose: contention names the blocker and
// explicitly rules OUT the missing-index reflex; a thin study refuses to
// conclude; sampled wording never claims exact timing.
func TestWaitsConclusion(t *testing.T) {
	contended := &model.WaitStudy{
		AAS: 2,
		Profile: &model.WaitProfile{Available: true, Samples: 50,
			Buckets: []model.WaitBucket{{Type: "Lock", Count: 40, Share: 0.8}, {Type: "CPU", Count: 10, Share: 0.2}}},
		Blockers: []model.Blocker{{HolderPID: 8172, HolderState: "idle in transaction",
			HolderXactAgeS: 43, Observations: 5, Sustained: true,
			Victims: []model.BlockedVictim{{PID: 18442}}}},
	}
	c := waitsConclusion(contended)
	for _, want := range []string{"contention", "8172", "not evidence of a missing index"} {
		if !strings.Contains(c, want) {
			t.Errorf("contended conclusion missing %q: %s", want, c)
		}
	}
	if strings.Contains(c, "exactly") {
		t.Errorf("conclusion must not claim exactness: %s", c)
	}

	thin := &model.WaitStudy{Thin: true,
		Profile: &model.WaitProfile{Available: true, Samples: 3}}
	if c := waitsConclusion(thin); !strings.Contains(c, "too few samples") {
		t.Errorf("thin study must refuse to conclude: %s", c)
	}

	idle := &model.WaitStudy{Profile: &model.WaitProfile{Available: true, Samples: 0}}
	if c := waitsConclusion(idle); !strings.Contains(c, "idle") {
		t.Errorf("no samples must read as idle: %s", c)
	}
}
