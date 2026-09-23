package findings

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/pgrundev/pgbot/internal/model"
)

func TestIdleTransactionAgeDoesNotBorrowActiveAge(t *testing.T) {
	for _, tc := range []struct {
		name     string
		idleAge  float64
		severity string
	}{
		{"fresh idle", 1, model.SeverityInfo},
		{"below warning", 59, model.SeverityInfo},
		{"warning boundary", 60, model.SeverityWarn},
		{"old idle", 120, model.SeverityWarn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var c model.Context
			input := fmt.Sprintf(`{"activity":{"idle_in_transaction":1,"longest_xact_sec":3600,"longest_idle_xact_sec":%g}}`, tc.idleAge)
			if err := json.Unmarshal([]byte(input), &c); err != nil {
				t.Fatal(err)
			}
			fs := Compute(&c)
			f := has(fs, "idle_in_transaction")
			if f == nil || f.Severity != tc.severity {
				t.Fatalf("idle age %g, active age 3600: got %+v; want %s", tc.idleAge, f, tc.severity)
			}
			if strings.Contains(f.Impact.Estimate, "3600") {
				t.Fatalf("idle finding borrowed the unrelated active transaction age: %+v", f.Impact)
			}
			if has(fs, "long_running_transaction") == nil {
				t.Fatal("the independently observed long active transaction must still be reported")
			}
		})
	}
}

func TestIdleTransactionWithoutIdleAgeDoesNotInventOne(t *testing.T) {
	c := &model.Context{Activity: &model.Activity{IdleInTransaction: 1, LongestXactSec: 3600}}
	f := has(Compute(c), "idle_in_transaction")
	if f == nil || f.Severity != model.SeverityInfo {
		t.Fatalf("older snapshots without an idle-specific age must not borrow global age: %+v", f)
	}
	if f.Impact.Score != 30 {
		t.Fatalf("unknown idle age must retain baseline impact: %+v", f.Impact)
	}
}
