package pglog

import (
	"context"
	"errors"
	"fmt"
	"path"

	"github.com/jackc/pgx/v5"
)

// RowQuerier is the one pgx capability the SQL source needs (satisfied by
// *pgxpool.Pool and *pgx.Conn).
type RowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// ErrNoCollector means the server writes no logfile pgbot can address —
// logging_collector is off (Docker images default to plain container stderr).
var ErrNoCollector = errors.New(
	"the server has no log collector: pg_current_logfile() is empty (logging_collector=off).\n" +
		"Enable it (logging_collector=on, and optionally jsonlog in log_destination), or on a\n" +
		"Docker container read the log with: docker logs <container>")

// SQLSource tails the server's own logfile over SQL: pg_current_logfile() to
// name it (pg_monitor), pg_ls_logdir() for its size (pg_monitor), and
// pg_read_binary_file() for content — the one call needing an extra GRANT.
type SQLSource struct {
	q RowQuerier
	// destination pins which log_destination we follow ('' = the primary),
	// chosen once so rotation re-resolves to the same format.
	destination string
}

// NewSQLSource picks the most machine-readable destination the server writes
// (jsonlog, then csvlog, then the primary stderr file) and returns a Source
// tailing it.
func NewSQLSource(ctx context.Context, q RowQuerier) (*SQLSource, error) {
	for _, dest := range []string{"jsonlog", "csvlog"} {
		var p *string
		err := q.QueryRow(ctx, "SELECT pg_current_logfile($1)", dest).Scan(&p)
		if err == nil && p != nil && *p != "" {
			return &SQLSource{q: q, destination: dest}, nil
		}
		if err != nil {
			return nil, fmt.Errorf("pg_current_logfile: %w", err)
		}
	}
	var p *string
	if err := q.QueryRow(ctx, "SELECT pg_current_logfile()").Scan(&p); err != nil {
		return nil, fmt.Errorf("pg_current_logfile: %w", err)
	}
	if p == nil || *p == "" {
		return nil, ErrNoCollector
	}
	return &SQLSource{q: q}, nil
}

func (s *SQLSource) Stat(ctx context.Context) (TailPos, error) {
	var name *string
	var err error
	if s.destination != "" {
		err = s.q.QueryRow(ctx, "SELECT pg_current_logfile($1)", s.destination).Scan(&name)
	} else {
		err = s.q.QueryRow(ctx, "SELECT pg_current_logfile()").Scan(&name)
	}
	if err != nil {
		return TailPos{}, fmt.Errorf("pg_current_logfile: %w", err)
	}
	if name == nil || *name == "" {
		return TailPos{}, ErrNoCollector
	}
	// pg_ls_logdir lists log_directory by bare filename; pg_current_logfile
	// returns a path (e.g. "log/postgresql-….json"). Match on the basename.
	var size int64
	err = s.q.QueryRow(ctx,
		"SELECT size FROM pg_ls_logdir() WHERE name = $1", path.Base(*name)).Scan(&size)
	if err != nil {
		return TailPos{}, fmt.Errorf("pg_ls_logdir: %w", err)
	}
	return TailPos{Name: *name, Size: size}, nil
}

func (s *SQLSource) ReadAt(ctx context.Context, name string, off, length int64) ([]byte, error) {
	var data []byte
	// missing_ok=true: a rotation between Stat and ReadAt must read as empty,
	// not error. The 4-arg form is the exact function the setup GRANT names.
	err := s.q.QueryRow(ctx,
		"SELECT pg_read_binary_file($1, $2, $3, true)", name, off, length).Scan(&data)
	if err != nil {
		return nil, fmt.Errorf("pg_read_binary_file: %w", err)
	}
	return data, nil
}

// ReadGrantSQL is the one statement `pgbot logs` needs beyond pg_monitor,
// printed when the server refuses the read.
func ReadGrantSQL(user string) string {
	return fmt.Sprintf(
		"GRANT EXECUTE ON FUNCTION pg_read_binary_file(text, bigint, bigint, boolean) TO %s;", user)
}
