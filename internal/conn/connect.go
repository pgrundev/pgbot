// Package conn opens the read-only, timeout-pinned connection pool to the target
// database, probes its version/extensions/provider capabilities, and provides the
// short READ ONLY transactions every collector and the advisor run inside.
package conn

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// clientOnlyParams are libpq connection parameters that pgx/pgconn does NOT
// recognize and so forwards to the server as startup GUCs — where the server
// rejects them ("unrecognized configuration parameter"). Managed providers
// (pgrun, Neon) ship channel_binding in their default connection strings. pgx
// can't implement SCRAM channel binding anyway, so we drop it; TLS from sslmode
// still applies — channel binding was hardening on top of that.
var clientOnlyParams = []string{"channel_binding"}

// Target is a configured, capability-probed connection to one database. The
// pool is small (max 4) so a burst of concurrent collectors can't itself become
// a connection storm on the database it was invoked to inspect.
type Target struct {
	Pool   *pgxpool.Pool
	Caps   Capabilities
	Pooler PoolerInfo
	self   *selfPIDs // backend PIDs of our own pool connections; see ExcludeSelf
}

const maxConns = 4

// Connect probes the server once, then builds a read-only pool whose sessions
// are pinned safe. The read-only GUARANTEE is the role (pg_monitor, no write
// grants); the session settings here are defence in depth, not a boundary.
func Connect(ctx context.Context, connString string) (*Target, error) {
	return ConnectDB(ctx, connString, "")
}

// ConnectDB is Connect with the target database overridden (for --all-databases):
// the connString supplies host/auth/TLS and `database` names which database on
// that server to inspect. Empty database keeps the connString's own.
func ConnectDB(ctx context.Context, connString, database string) (*Target, error) {
	cfg, err := pgxpool.ParseConfig(connString)
	if err != nil {
		return nil, fmt.Errorf("parse connection string: %w", err)
	}
	if database != "" {
		cfg.ConnConfig.Database = database
	}
	cfg.MaxConns = maxConns
	cfg.MinConns = 0
	cfg.MaxConnLifetime = 5 * time.Minute
	cfg.ConnConfig.RuntimeParams["application_name"] = "pgbot"

	// Route the TCP leg through the SSH jump host when one is configured. This has
	// to happen before probe(): the probe connection dials too, and it must take
	// the same path as the pool. Installing it here rather than rewriting the DSN
	// to a local forward is what keeps sslmode= and .pgpass matching on the real
	// hostname — see sshtunnel.go.
	if dial := sshDialFunc(); dial != nil {
		cfg.ConnConfig.DialFunc = dial
	}

	// Drop client-only params pgx forwarded into RuntimeParams (it would send them
	// as server GUCs, which the server rejects). See clientOnlyParams.
	for _, p := range clientOnlyParams {
		if _, ok := cfg.ConnConfig.RuntimeParams[p]; ok {
			delete(cfg.ConnConfig.RuntimeParams, p)
			fmt.Fprintf(os.Stderr, "pgbot: ignoring connection param %q — the driver can't honor it; TLS from sslmode still applies\n", p)
		}
	}

	// Probe capabilities + pooler on a throwaway connection first, so AfterConnect
	// applies only the GUCs this server understands and the pool uses the right
	// wire protocol.
	caps, pooler, err := probe(ctx, cfg.ConnConfig.Copy())
	if err != nil {
		return nil, err
	}
	if pooler.SimpleProtocol {
		cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	}

	// Track our own backend PIDs so collectors can exclude every pgbot connection
	// from pg_stat_activity, not just the one running a given query (ExcludeSelf).
	self := newSelfPIDs()
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		if err := applySessionSetup(ctx, c, caps); err != nil {
			return err
		}
		self.add(c.PgConn().PID())
		return nil
	}
	cfg.BeforeClose = func(c *pgx.Conn) {
		self.remove(c.PgConn().PID())
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	return &Target{Pool: pool, Caps: caps, Pooler: pooler, self: self}, nil
}

func (t *Target) Close() {
	if t.Pool != nil {
		t.Pool.Close()
	}
}

// Warm opens the pool to its full size and releases the connections back, so
// every one of pgbot's backend PIDs is registered (ExcludeSelf) BEFORE any
// collector samples pg_stat_activity. Without this, collectors that open pool
// connections concurrently could sample while a sibling is still being created —
// its PID not yet tracked — and count it as a phantom idle-in-transaction
// session. Acquiring all connections at once (before releasing any) forces the
// pool to open distinct ones. Best-effort: an Acquire failure just leaves the
// pool partly warm, which degrades to the pg_backend_pid()-only filter.
func (t *Target) Warm(ctx context.Context) {
	if t.Pool == nil {
		return
	}
	held := make([]*pgxpool.Conn, 0, maxConns)
	for i := 0; i < maxConns; i++ {
		c, err := t.Pool.Acquire(ctx)
		if err != nil {
			break
		}
		held = append(held, c)
	}
	for _, c := range held {
		c.Release()
	}
}

// sessionPins are the GUCs applySessionSetup sets on every pgbot session
// (application_name is pinned too, but separately — it is pgbot's identity in
// pg_stat_activity and must never be unpinned). Anything that reports the SERVER's
// configuration must look past these: pg_settings.setting and current_setting()
// otherwise show THIS session's pinned values, not the database's (e.g.
// statement_timeout=15s on a server that has none).
var sessionPins = []struct{ name, value string }{
	{"statement_timeout", "'15s'"},
	{"lock_timeout", "'2s'"},
	{"idle_in_transaction_session_timeout", "'10s'"},
	{"default_transaction_read_only", "on"},
}

// UnpinLocal reverts pgbot's session pins for the current transaction only
// (SET LOCAL … = DEFAULT restores the server/database/role value the session would
// have without our SET). COMMIT re-establishes the pins, so a caller gets one
// transaction in which pg_settings and current_setting() describe the database
// rather than pgbot. Only the settings collector needs it. stats_fetch_consistency
// (PG15+, also pinned) is left alone: it isn't a tuning parameter, and a SET LOCAL
// of an unknown GUC would abort the transaction on PG < 15.
func UnpinLocal(ctx context.Context, tx pgx.Tx) error {
	for _, p := range sessionPins {
		if _, err := tx.Exec(ctx, "SET LOCAL "+p.name+" = DEFAULT"); err != nil {
			return fmt.Errorf("unpin %s: %w", p.name, err)
		}
	}
	return nil
}

// applySessionSetup pins every physical connection. statement_timeout and
// lock_timeout are mandatory: pgbot must never become the incident it was
// invoked to diagnose.
func applySessionSetup(ctx context.Context, c *pgx.Conn, caps Capabilities) error {
	stmts := []string{"SET application_name = 'pgbot'"}
	for _, p := range sessionPins {
		stmts = append(stmts, "SET "+p.name+" = "+p.value)
	}
	for _, s := range stmts {
		if _, err := c.Exec(ctx, s); err != nil {
			return fmt.Errorf("session setup %q: %w", s, err)
		}
	}
	// PG15+ only. Without it, stats views are cached for the transaction's
	// lifetime and a double-sample inside one transaction would read identical
	// counters (every rate zero). We ALSO sample in separate transactions, so
	// this is belt-and-suspenders; ignore the error on older servers.
	if caps.HasStatsFetchConsistency() {
		_, _ = c.Exec(ctx, "SET stats_fetch_consistency = 'none'")
	}
	return nil
}

// probe reads server_version_num, installed extensions, role membership, and
// the system identifier in one round trip (with a best-effort fallback for the
// identifier, which needs elevated read access on some managed providers).
func probe(ctx context.Context, cc *pgx.ConnConfig) (Capabilities, PoolerInfo, error) {
	c, err := pgx.ConnectConfig(ctx, cc)
	if err != nil {
		return Capabilities{}, PoolerInfo{}, fmt.Errorf("connect: %w", err)
	}
	defer c.Close(ctx)

	// Detect the pooler first — if prepared statements are broken, every later
	// query on this probe connection must use the simple protocol too.
	pooler := detectPooler(ctx, c, cc)
	mode := []any{}
	if pooler.SimpleProtocol {
		mode = []any{pgx.QueryExecModeSimpleProtocol}
	}

	var caps Capabilities
	var mk providerMarkers
	const q = `
		SELECT current_setting('server_version_num')::int,
		       version(),
		       current_database(),
		       pg_postmaster_start_time(),
		       (SELECT count(*) FROM pg_extension WHERE extname = 'pg_stat_statements') > 0,
		       (SELECT count(*) FROM pg_extension WHERE extname = 'hypopg') > 0,
		       pg_has_role(current_user, 'pg_monitor', 'MEMBER'),
		       (SELECT count(*) FROM pg_settings WHERE name LIKE 'rds.%') > 0,
		       (SELECT count(*) FROM pg_settings WHERE name LIKE 'cloudsql.%') > 0,
		       (SELECT count(*) FROM pg_settings WHERE name LIKE 'azure.%') > 0,
		       pg_is_in_recovery(),
		       -- Aurora exposes aurora_version(). Look it up in the catalog rather
		       -- than CALLING it: on every other server the call fails, which writes
		       -- an ERROR to the server log and books a rollback in pg_stat_database
		       -- on each pgbot run — the very counter pgbot reports.
		       (SELECT count(*) FROM pg_proc WHERE proname = 'aurora_version') > 0`
	err = c.QueryRow(ctx, q, mode...).Scan(&caps.VersionNum, &caps.VersionText, &caps.Database,
		&caps.StartedAt, &caps.HasStatStatements, &caps.HasHypopg, &caps.HasPgMonitor,
		&mk.HasRDS, &mk.HasCloudSQL, &mk.HasAzure, &caps.InRecovery, &mk.IsAurora)
	if err != nil {
		return Capabilities{}, pooler, fmt.Errorf("probe capabilities: %w", err)
	}
	caps.RecoveryChecked = true // the probe scan succeeded, so InRecovery is trustworthy
	mk.Host, mk.VersionText = cc.Host, caps.VersionText
	caps.Provider = detectProvider(mk)

	// system_identifier makes the baseline fingerprint survive a restore/rename;
	// it needs pg_monitor/superuser on some providers, so it's best-effort.
	var sysID int64
	if err := c.QueryRow(ctx, `SELECT system_identifier FROM pg_control_system()`, mode...).Scan(&sysID); err == nil {
		caps.SystemIdentifier = fmt.Sprintf("%d", sysID)
	}

	// Installed extensions WITH the namespace each lives in. The schema is what
	// lets every extension read (pg_stat_statements, hypopg) address its objects
	// by qualified name — Supabase and friends install them in "extensions", off
	// the read-only role's search_path (issue #10). Best-effort: on failure the
	// map stays empty and callers fall back to bare names.
	if rows, err := c.Query(ctx, `SELECT e.extname, n.nspname FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace ORDER BY e.extname`, mode...); err == nil {
		type extRow struct {
			Name   string
			Schema string
		}
		if exts, err := pgx.CollectRows(rows, pgx.RowToStructByPos[extRow]); err == nil {
			caps.ExtensionSchemas = make(map[string]string, len(exts))
			for _, e := range exts {
				caps.Extensions = append(caps.Extensions, e.Name)
				caps.ExtensionSchemas[e.Name] = e.Schema
			}
		}
	}
	return caps, pooler, nil
}

// ReadOnlyTx runs fn inside its own short READ ONLY transaction and always rolls
// back. Each collector sample gets a fresh transaction — that, plus
// stats_fetch_consistency='none', is what keeps double-sampled rates non-zero.
func (t *Target) ReadOnlyTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := t.Pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback(context.Background())
		return err
	}
	// COMMIT the read-only probe rather than rolling it back. A read-only txn
	// writes nothing either way, but rolling back would increment the very
	// xact_rollback counter pgbot reports — on a quiet database, pgbot observing
	// itself would manufacture a "high rollback ratio" finding.
	return tx.Commit(ctx)
}
