package collect

import (
	"context"
	_ "embed"
	"time"

	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/model"
)

//go:embed sql/collation.sql
var sqlCollation string

// collation = catalog objects whose recorded collation version no longer matches
// the library the server runs against. PG15+, when pg_database began recording
// datcollversion. Empty list = healthy.
type collationCollector struct{}

type collationRow struct {
	Kind     string `db:"kind"`
	Name     string `db:"name"`
	Provider string `db:"provider"`
	Recorded string `db:"recorded"`
	Actual   string `db:"actual"`
}

func (collationCollector) Name() string { return "collation" }
func (collationCollector) Kind() Kind   { return KindGauge }
func (collationCollector) Available(caps conn.Capabilities) bool {
	return caps.VersionNum >= 150000 // pg_database.datcollversion + pg_database_collation_actual_version()
}

func (collationCollector) Sample(ctx context.Context, t *conn.Target, _ conn.Capabilities) (any, error) {
	return queryMany[collationRow](ctx, t, sqlCollation)
}

func (collationCollector) Assemble(c *model.Context, _ conn.Capabilities, s sampled, _ time.Duration, _ Options) {
	rows, ok := s.A.([]collationRow)
	if s.Err != nil || !ok {
		c.Collation = &model.Collation{Section: unavail(s.Err, "collation versions need PostgreSQL 15+")}
		return
	}
	col := &model.Collation{Section: model.Section{Exactness: model.ExactnessScraped}}
	for _, r := range rows {
		col.Mismatches = append(col.Mismatches, model.CollationMismatch{
			Kind: r.Kind, Name: r.Name, Provider: r.Provider, Recorded: r.Recorded, Actual: r.Actual,
		})
	}
	c.Collation = col
}
