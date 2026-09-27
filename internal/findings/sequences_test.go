package findings

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/pgrundev/pgbot/internal/model"
)

func TestSequenceExhaustion(t *testing.T) {
	ok := &model.Context{Sequences: &model.Sequences{Items: []model.SequenceUsage{
		{Schema: "public", Name: "s1", LastValue: 1000, Ceiling: 2147483647, PctUsed: 0.0000005},
	}}}
	if has(Compute(ok), "sequence_exhaustion") != nil {
		t.Error("a fresh sequence must not fire")
	}
	warn := &model.Context{Sequences: &model.Sequences{Items: []model.SequenceUsage{
		{Schema: "public", Name: "orders_id_seq", LastValue: 1_800_000_000, Ceiling: 2_147_483_647, PctUsed: 0.838, OwnedBy: "orders.id"},
	}}}
	f := has(Compute(warn), "sequence_exhaustion")
	if f == nil || f.Severity != model.SeverityWarn || f.Impact.Dimension != model.DimRisk {
		t.Fatalf("expected warn/risk, got %+v", f)
	}
	crit := &model.Context{Sequences: &model.Sequences{Items: []model.SequenceUsage{
		{Schema: "public", Name: "s", LastValue: 2_000_000_000, Ceiling: 2_147_483_647, PctUsed: 0.93},
	}}}
	if f := has(Compute(crit), "sequence_exhaustion"); f == nil || f.Severity != model.SeverityCritical {
		t.Errorf("expected critical, got %+v", f)
	}
}

func TestSequenceExhaustionMetadataSurvivesJSONRoundTrip(t *testing.T) {
	var c model.Context
	if err := json.Unmarshal([]byte(`{
		"sequences": {"items": [
			{"schema":"public","sequence":"descending","last_value":-95,"floor":-100,"ceiling":-1,"pct_used":0.9495,"increment":-1},
			{"schema":"public","sequence":"safe_cycle","last_value":99,"floor":1,"ceiling":100,"pct_used":0.9899,"increment":1,"cycle":true},
			{"schema":"public","sequence":"narrow_wrap_cycle","last_value":-2000000000,"floor":-2147483648,"ceiling":2147483647,"pct_used":0.9657,"increment":-1,"cycle":true,"column_limited":true}
		]}
	}`), &c); err != nil {
		t.Fatal(err)
	}

	fixedBefore, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal enriched Context: %v", err)
	}
	findingsBefore := Compute(&c)
	fixedAfter, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal Context after Compute: %v", err)
	}
	if !bytes.Equal(fixedBefore, fixedAfter) {
		t.Fatal("Compute mutated its input Context")
	}
	if !json.Valid(fixedAfter) {
		t.Fatalf("Context JSON is invalid: %s", fixedAfter)
	}

	var roundTripped model.Context
	if err := json.Unmarshal(fixedAfter, &roundTripped); err != nil {
		t.Fatalf("unmarshal enriched Context: %v", err)
	}
	findingsAfter := Compute(&roundTripped)
	if !reflect.DeepEqual(findingsBefore, findingsAfter) {
		t.Fatalf("findings changed across Context JSON round trip:\nbefore: %+v\nafter:  %+v", findingsBefore, findingsAfter)
	}

	f := has(findingsAfter, "sequence_exhaustion")
	if f == nil {
		t.Fatal("expected sequence_exhaustion")
	}
	wantObjects := []string{"public.descending", "public.narrow_wrap_cycle"}
	if !reflect.DeepEqual(f.Objects, wantObjects) {
		t.Fatalf("safe in-range cycle must be excluded while column-limited cycle remains: got %v want %v", f.Objects, wantObjects)
	}
}

func TestSequenceExhaustionLegacyContextKeepsStoredPctBehavior(t *testing.T) {
	var c model.Context
	if err := json.Unmarshal([]byte(`{
		"sequences": {"items": [{
			"schema":"public","sequence":"legacy","last_value":2000000000,
			"ceiling":2147483647,"pct_used":0.93,"cycle":true
		}]}
	}`), &c); err != nil {
		t.Fatal(err)
	}
	f := has(Compute(&c), "sequence_exhaustion")
	if f == nil || f.Severity != model.SeverityCritical {
		t.Fatalf("increment=0 must preserve legacy pct_used behavior, got %+v", f)
	}
	wantEvidence := "public.legacy: 93% used (2.0G / 2.1G)"
	if len(f.Evidence) != 1 || f.Evidence[0] != wantEvidence {
		t.Fatalf("legacy evidence changed: got %v want %q", f.Evidence, wantEvidence)
	}
}
