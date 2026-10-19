package collect

import (
	"encoding/json"
	"testing"

	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/model"
)

func TestActivityAssembleKeepsIdleAgeSeparate(t *testing.T) {
	for _, state := range []string{"idle in transaction", "idle in transaction (aborted)"} {
		t.Run(state, func(t *testing.T) {
			var c model.Context
			activityCollector{}.Assemble(&c, conn.Capabilities{}, sampled{A: activitySample{Rows: []activityRow{
				{State: "active", N: 1, MaxXactAgeS: 3600, MaxActiveAgeS: 500},
				{State: state, N: 2, MaxXactAgeS: 120},
				{State: "idle in transaction", N: 1, MaxXactAgeS: 1},
			}}}, 0, Options{})
			if c.Activity.IdleInTransaction != 3 || c.Activity.LongestXactSec != 3600 || c.Activity.LongestActiveSec != 500 {
				t.Fatalf("existing activity measurements changed: %+v", c.Activity)
			}
			data, err := json.Marshal(c.Activity)
			if err != nil {
				t.Fatal(err)
			}
			var out map[string]any
			if err := json.Unmarshal(data, &out); err != nil {
				t.Fatal(err)
			}
			if out["longest_idle_xact_sec"] != float64(120) {
				t.Fatalf("idle-specific age = %v, want 120 (not active age 3600)", out["longest_idle_xact_sec"])
			}
		})
	}
}

func TestActivityAssembleWithoutIdleDoesNotBorrowAge(t *testing.T) {
	var c model.Context
	activityCollector{}.Assemble(&c, conn.Capabilities{}, sampled{A: activitySample{Rows: []activityRow{
		{State: "active", N: 1, MaxXactAgeS: 3600},
	}}}, 0, Options{})
	data, err := json.Marshal(c.Activity)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if _, exists := out["longest_idle_xact_sec"]; exists {
		t.Fatalf("no idle transaction: optional idle age should be absent, got %s", data)
	}
}
