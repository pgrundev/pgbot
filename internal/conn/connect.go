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
}

const maxConns = 4

// Connect probes the server once, then builds a read-only pool whose sessions
// are pinned safe. The read-only GUARANTEE is the role (pg_monitor, no write
// grants); the session settings here are defence in depth, not a boundary.
func Connect(ctx context.Context, connString string) (*Target, error) {
	cfg, err := pgxpool.ParseConfig(connString)
	if err != nil {
		return nil, fmt.Errorf("parse connection string: %w", err)
	}
	cfg.MaxConns = maxConns
	cfg.MinConns = 0
	cfg.MaxConnLifetime = 5 * time.Minute
	cfg.ConnConfig.RuntimeParams["application_name"] = "pgbot"

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

	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		return applySessionSetup(ctx, c, caps)
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	return &Target{Pool: pool, Caps: caps, Pooler: pooler}, nil
}

func (t *Target) Close() {
	if t.Pool != nil {
		t.Pool.Close()
	}
}

// AppName is the application_name every pgbot session carries. Every read of
// pg_stat_activity filters on it (`application_name IS DISTINCT FROM 'pgbot'`)
// so pgbot never counts its own pool or wait-sampler connections as the
// database's activity — connections, idle-in-transaction, xmin holders, ASH.
// A `pid <> pg_backend_pid()` filter only hides the one backend running that
// query; the rest of the pool is still visible to it.
const AppName = "pgbot"

// applySessionSetup pins every physical connection. statement_timeout and
// lock_timeout are mandatory: pgbot must never become the incident it was
// invoked to diagnose.
func applySessionSetup(ctx context.Context, c *pgx.Conn, caps Capabilities) error {
	stmts := []string{
		"SET application_name = '" + AppName + "'",
		"SET statement_timeout = '15s'",
		"SET lock_timeout = '2s'",
		"SET idle_in_transaction_session_timeout = '10s'",
		"SET default_transaction_read_only = on",
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
		       pg_is_in_recovery()`
	err = c.QueryRow(ctx, q, mode...).Scan(&caps.VersionNum, &caps.VersionText, &caps.Database,
		&caps.StartedAt, &caps.HasStatStatements, &caps.HasHypopg, &caps.HasPgMonitor,
		&mk.HasRDS, &mk.HasCloudSQL, &mk.HasAzure, &caps.InRecovery)
	if err != nil {
		return Capabilities{}, pooler, fmt.Errorf("probe capabilities: %w", err)
	}
	caps.RecoveryChecked = true // the probe scan succeeded, so InRecovery is trustworthy
	// Aurora exposes aurora_version(); a failure just means "not Aurora".
	var av string
	mk.IsAurora = c.QueryRow(ctx, `SELECT aurora_version()`, mode...).Scan(&av) == nil
	mk.Host, mk.VersionText = cc.Host, caps.VersionText
	caps.Provider = detectProvider(mk)

	// system_identifier makes the baseline fingerprint survive a restore/rename;
	// it needs pg_monitor/superuser on some providers, so it's best-effort.
	var sysID int64
	if err := c.QueryRow(ctx, `SELECT system_identifier FROM pg_control_system()`, mode...).Scan(&sysID); err == nil {
		caps.SystemIdentifier = fmt.Sprintf("%d", sysID)
	}

	if rows, err := c.Query(ctx, `SELECT extname FROM pg_extension ORDER BY extname`, mode...); err == nil {
		if exts, err := pgx.CollectRows(rows, pgx.RowTo[string]); err == nil {
			caps.Extensions = exts
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
