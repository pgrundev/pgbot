package collect

import (
	"context"
	_ "embed"
	"time"

	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/model"
)

//go:embed sql/replident.sql
var sqlReplident string

// replident = published tables whose replica identity can't identify a row, so
// UPDATE/DELETE on them errors. Catalog-only, so it works on an empty database.
type replidentCollector struct{}

type replidentRow struct {
	Schema       string `db:"schema"`
	Table        string `db:"table"`
	Identity     string `db:"identity"`
	Publications string `db:"publications"`
}

func (replidentCollector) Name() string                     { return "replident" }
func (replidentCollector) Kind() Kind                       { return KindGauge }
func (replidentCollector) Available(conn.Capabilities) bool { return true }

func (replidentCollector) Sample(ctx context.Context, t *conn.Target, _ conn.Capabilities) (any, error) {
	return queryMany[replidentRow](ctx, t, sqlReplident)
}

func (replidentCollector) Assemble(c *model.Context, _ conn.Capabilities, s sampled, _ time.Duration, _ Options) {
	rows, ok := s.A.([]replidentRow)
	if s.Err != nil || !ok {
		c.ReplicaIdentity = &model.ReplicaIdentity{Section: unavail(s.Err, "publication catalog unreadable")}
		return
	}
	ri := &model.ReplicaIdentity{Section: model.Section{Exactness: model.ExactnessScraped}}
	for _, r := range rows {
		ri.Unidentifiable = append(ri.Unidentifiable, model.PublishedTable{
			Schema: r.Schema, Name: r.Table, Identity: r.Identity, Publications: r.Publications,
		})
	}
	c.ReplicaIdentity = ri
}
