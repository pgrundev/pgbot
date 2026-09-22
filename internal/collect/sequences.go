package collect

import (
	"context"
	_ "embed"
	"time"

	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/model"
)

//go:embed sql/sequences.sql
var sqlSequences string

//go:embed sql/narrow_identity.sql
var sqlNarrowIdentity string

// sequences = per-sequence position in the increment direction, intersecting
// configured bounds with the owning column's integer range. PG10+ (pg_sequences).
// It also collects the structural half — narrow (int2/int4) sequence-backed
// columns — which is schema-scoped and holds on an empty database.
type sequencesCollector struct{}

type sequenceRow struct {
	Schema        string `db:"schema"`
	Name          string `db:"sequence"`
	LastValue     int64  `db:"last_value"`
	Floor         int64  `db:"floor"`
	Ceiling       int64  `db:"ceiling"`
	Increment     int64  `db:"increment"`
	Cycle         bool   `db:"cycle"`
	ColumnLimited bool   `db:"column_limited"`
	OwnedBy       string `db:"owned_by"`
}

type narrowIdentityRow struct {
	Schema string `db:"schema"`
	Table  string `db:"table"`
	Column string `db:"column"`
	Type   string `db:"type"`
}

type sequencesSample struct {
	Seqs   []sequenceRow
	Narrow []narrowIdentityRow
}

func (sequencesCollector) Name() string { return "sequences" }
func (sequencesCollector) Kind() Kind   { return KindGauge }
func (sequencesCollector) Available(caps conn.Capabilities) bool {
	return caps.VersionNum >= 100000 // pg_sequences + attidentity
}

func (sequencesCollector) Sample(ctx context.Context, t *conn.Target, _ conn.Capabilities) (any, error) {
	seqs, err := queryMany[sequenceRow](ctx, t, sqlSequences)
	if err != nil {
		return nil, err
	}
	narrow, _ := queryMany[narrowIdentityRow](ctx, t, sqlNarrowIdentity) // best-effort
	return sequencesSample{Seqs: seqs, Narrow: narrow}, nil
}

func (sequencesCollector) Assemble(c *model.Context, _ conn.Capabilities, s sampled, _ time.Duration, _ Options) {
	ss, ok := s.A.(sequencesSample)
	if s.Err != nil || !ok {
		c.Sequences = &model.Sequences{Section: unavail(s.Err, "sequence usage unavailable")}
		return
	}
	seq := &model.Sequences{Section: model.Section{Exactness: model.ExactnessScraped}}
	for _, r := range ss.Seqs {
		seq.Items = append(seq.Items, model.SequenceUsage{
			Schema: r.Schema, Name: r.Name, LastValue: r.LastValue, Ceiling: r.Ceiling,
			PctUsed: round4(sequencePctUsed(r)), OwnedBy: r.OwnedBy,
			Floor: r.Floor, Increment: r.Increment, Cycle: r.Cycle, ColumnLimited: r.ColumnLimited,
		})
	}
	for _, r := range ss.Narrow {
		seq.NarrowIdentity = append(seq.NarrowIdentity, model.NarrowIdentityColumn{
			Schema: r.Schema, Table: r.Table, Column: r.Column, Type: r.Type,
		})
	}
	c.Sequences = seq
}

func sequencePctUsed(r sequenceRow) float64 {
	if r.Increment == 0 || r.Floor >= r.Ceiling || r.LastValue < r.Floor || r.LastValue > r.Ceiling {
		return 1
	}
	span := uint64(r.Ceiling) - uint64(r.Floor)
	var position uint64
	if r.Increment < 0 {
		position = uint64(r.Ceiling) - uint64(r.LastValue)
	} else {
		position = uint64(r.LastValue) - uint64(r.Floor)
	}
	return float64(position) / float64(span)
}
