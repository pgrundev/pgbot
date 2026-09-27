package model

import (
	"encoding/json"
	"testing"
)

func TestSequenceMetadataIsOptionalAndRoundTrips(t *testing.T) {
	input := []byte(`{
		"sequences": {"items": [
			{"schema":"public","sequence":"enriched","last_value":-9,"floor":-10,"ceiling":10,"pct_used":0.95,"increment":-1,"cycle":true,"column_limited":true,"owned_by":"t.id"},
			{"schema":"public","sequence":"zero_values","last_value":0,"ceiling":10,"pct_used":0,"increment":1},
			{"schema":"public","sequence":"legacy","last_value":9,"ceiling":10,"pct_used":0.9}
		]}
	}`)
	var c Context
	if err := json.Unmarshal(input, &c); err != nil {
		t.Fatal(err)
	}
	blob, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(blob, &raw); err != nil {
		t.Fatal(err)
	}
	items := raw["sequences"].(map[string]any)["items"].([]any)
	enriched := items[0].(map[string]any)
	for _, key := range []string{"ceiling", "pct_used", "floor", "increment", "cycle", "column_limited"} {
		if _, ok := enriched[key]; !ok {
			t.Errorf("enriched sequence lost %q after round trip: %s", key, blob)
		}
	}
	zeroValues := items[1].(map[string]any)
	if _, ok := zeroValues["floor"]; ok {
		t.Errorf("zero floor must remain optional with omitempty: %s", blob)
	}
	if _, ok := zeroValues["cycle"]; ok {
		t.Errorf("false cycle must remain optional with omitempty: %s", blob)
	}
	legacy := items[2].(map[string]any)
	for _, key := range []string{"floor", "increment", "cycle", "column_limited"} {
		if _, ok := legacy[key]; ok {
			t.Errorf("legacy sequence unexpectedly gained %q: %s", key, blob)
		}
	}
}
