package collect

import (
	"context"
	_ "embed"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/model"
)

//go:embed sql/settings.sql
var sqlSettings string

// settings = parameters whose active value differs from the compiled-in default.
type settingsCollector struct{}

type settingRow struct {
	Name  string `db:"name"`
	Value string `db:"value"`
	Kind  string `db:"kind"`
}

func (settingsCollector) Name() string                     { return "settings" }
func (settingsCollector) Kind() Kind                       { return KindGauge }
func (settingsCollector) Available(conn.Capabilities) bool { return true }

func (settingsCollector) Sample(ctx context.Context, t *conn.Target, _ conn.Capabilities) (any, error) {
	// Report the SERVER's configuration, not this session's. pgbot pins
	// statement_timeout, lock_timeout, idle_in_transaction_session_timeout,
	// default_transaction_read_only and stats_fetch_consistency on every
	// connection; read through those pins they show up as "non-default
	// parameters" (statement_timeout=15s on a server that has none) and any rule
	// keyed on them evaluates pgbot instead of the database. UnpinLocal reverts
	// them for this one transaction; the query below additionally drops
	// session/client-sourced rows (application_name) from the override set.
	var rows []settingRow
	err := t.ReadOnlyTx(ctx, func(tx pgx.Tx) error {
		if err := conn.UnpinLocal(ctx, tx); err != nil {
			return err
		}
		r, err := tx.Query(ctx, sqlSettings)
		if err != nil {
			return err
		}
		rows, err = pgx.CollectRows(r, pgx.RowToStructByNameLax[settingRow])
		return err
	})
	return rows, err
}

func (settingsCollector) Assemble(c *model.Context, _ conn.Capabilities, s sampled, _ time.Duration, _ Options) {
	rows, ok := s.A.([]settingRow)
	if s.Err != nil || !ok {
		c.Settings = &model.Settings{Section: unavail(s.Err, "pg_settings unavailable")}
		return
	}
	set := &model.Settings{Section: model.Section{Exactness: model.ExactnessScraped}, Overrides: map[string]string{}, Params: map[string]string{}}
	for _, r := range rows {
		if r.Kind == "tuning" {
			set.Params[r.Name] = r.Value
		} else {
			set.Overrides[r.Name] = r.Value
		}
	}
	c.Settings = set
}
