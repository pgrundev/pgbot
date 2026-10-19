package plan

import (
	"context"
	"errors"
	"fmt"

	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pggo"
)

// ErrNotSelect is returned (wrapped) when the query is not a single plain SELECT.
var ErrNotSelect = errors.New("only a single plain SELECT can be explained — no writes, no WITH, no multiple statements, no utility statements")

// Fetch runs the plan-only EXPLAIN for query on target and returns the raw
// FORMAT JSON output. generic reports whether GENERIC_PLAN was used (PG16+),
// which the caller should say, since those estimates assume unknown parameter
// values. The query is planned inside a READ ONLY transaction and never
// executed; Prepare has already refused anything but one plain SELECT.
func Fetch(ctx context.Context, target *conn.Target, query string) (planJSON []byte, generic bool, err error) {
	stmt, err := Prepare(query, target.Caps.VersionNum)
	if err != nil {
		return nil, false, err
	}
	generic = target.Caps.VersionNum >= 160000
	err = target.ReadOnlyTx(ctx, func(tx *pggo.Tx) error {
		res, e := tx.Conn().SimpleQuery(ctx, stmt)
		if e != nil {
			return e
		}
		for _, r := range res {
			if len(r.Rows) > 0 && len(r.Rows[0]) > 0 {
				planJSON = r.Rows[0][0]
				return nil
			}
		}
		return fmt.Errorf("no plan returned")
	})
	if err != nil {
		return nil, generic, fmt.Errorf("explain: %s", conn.RedactConnString(err.Error()))
	}
	return planJSON, generic, nil
}
