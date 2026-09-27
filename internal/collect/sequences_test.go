package collect

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/model"
)

func TestSequencesCollectorComputesDirectionAwarePositionWithoutFloatCancellation(t *testing.T) {
	tests := []struct {
		name                 string
		last, floor, ceiling int64
		increment            int64
		want                 float64
	}{
		{"narrow range near MaxInt64", math.MaxInt64 - 1, math.MaxInt64 - 10, math.MaxInt64, 1, 0.9},
		{"narrow range near MinInt64", math.MinInt64 + 1, math.MinInt64, math.MinInt64 + 10, -1, 0.9},
		{"full signed span ascending", 0, math.MinInt64, math.MaxInt64, 1, 0.5},
		{"full signed span descending", -1, math.MinInt64, math.MaxInt64, -1, 0.5},
		{"crosses zero ascending", 0, -10, 10, 1, 0.5},
		{"crosses zero descending", 0, -10, 10, -1, 0.5},
		{"zero lower bound", 80, 0, 100, 1, 0.8},
		{"below effective range", -11, -10, 10, 1, 1},
		{"above effective range", 11, -10, 10, -1, 1},
		{"unordered effective range", 5, 10, 0, 1, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			row := sequenceRow{Schema: "fixture", Name: "s", LastValue: tt.last, Ceiling: tt.ceiling}
			setOptionalSequenceRowField(&row, "Floor", tt.floor)
			setOptionalSequenceRowField(&row, "Increment", tt.increment)

			c := &model.Context{}
			(sequencesCollector{}).Assemble(c, conn.Capabilities{}, sampled{
				A: sequencesSample{Seqs: []sequenceRow{row}},
			}, 0, Options{})
			if c.Sequences == nil || len(c.Sequences.Items) != 1 {
				t.Fatalf("collector omitted sequence with bounds [%d, %d]: %+v", tt.floor, tt.ceiling, c.Sequences)
			}
			got := c.Sequences.Items[0].PctUsed
			if math.IsNaN(got) || math.IsInf(got, 0) {
				t.Fatalf("pct_used must be finite, got %v", got)
			}
			if _, err := json.Marshal(c); err != nil {
				t.Fatalf("Context JSON must remain finite: %v", err)
			}
			if math.Abs(got-tt.want) > 0.0001 {
				t.Errorf("pct_used = %v, want %v", got, tt.want)
			}
		})
	}
}

// setOptionalSequenceRowField keeps this regression runnable against the
// original struct so RED demonstrates wrong runtime output, not a compile error.
func setOptionalSequenceRowField(row *sequenceRow, name string, value any) {
	f := reflect.ValueOf(row).Elem().FieldByName(name)
	if f.IsValid() && f.CanSet() {
		f.Set(reflect.ValueOf(value))
	}
}

func TestSequencesSQLBoundsAndRanksBeforeItsLimit(t *testing.T) {
	for _, want := range []string{
		"WITH ranges AS",
		"LIMIT 50",
		"last_value::numeric - floor::numeric",
		"ceiling::numeric - last_value::numeric",
		"schema, sequence",
	} {
		if !strings.Contains(sqlSequences, want) {
			t.Errorf("sequence SQL missing %q", want)
		}
	}
	if strings.Contains(strings.ToLower(sqlSequences), "nextval(") {
		t.Error("sequence collector must never advance an inspected sequence")
	}
	if strings.Index(sqlSequences, "ORDER BY") > strings.Index(sqlSequences, "LIMIT 50") {
		t.Error("sequence risk ordering must happen before the bounded LIMIT")
	}
}
