// Package conn opens the read-only, timeout-pinned connection pool to the target
// database, probes its version/extensions/provider capabilities, and provides the
// short READ ONLY transactions every collector and the advisor run inside.
package conn

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/pgrundev/pggo"
)

// clientOnlyParams are libpq connection parameters the driver accepts but cannot
// honor. Managed providers (pgrun, Neon) ship channel_binding in their default
// connection strings; pgGo does not implement SCRAM channel binding, so it is
// dropped (never sent as a server GUC) and we say so. TLS from sslmode still
// applies — channel binding was hardening on top of that.
var clientOnlyParams = []string{"channel_binding"}

// hasConnParam reports whether a URL or keyword=value connection string sets key.
func hasConnParam(connString, key string) bool {
	if strings.HasPrefix(connString, "postgres://") || strings.HasPrefix(connString, "postgresql://") {
		u, err := url.Parse(connString)
		return err == nil && u.Query().Has(key)
	}
	for _, f := range strings.Fields(connString) {
		if strings.HasPrefix(f, key+"=") {
			return true
		}
	}
	return false
}

// Target is a configured, capability-probed connection to one database. The
// pool is small (max 4) so a burst of concurrent collectors can't itself become
// a connection storm on the database it was invoked to inspect.
type Target struct {
	Pool   *pggo.Pool
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
	return connect(ctx, connString, database, "")
}

// ConnectDBAt is ConnectDB with the host overridden as well (for
// --all-instances): the connString supplies auth and TLS settings, `host` names
// which cluster member to reach. The TLS server name follows the host, so
// sslmode=verify-full validates that member's own certificate.
func ConnectDBAt(ctx context.Context, connString, database, host string) (*Target, error) {
	return connect(ctx, connString, database, host)
}

func connect(ctx context.Context, connString, database, host string) (*Target, error) {
	cfg, err := pggo.ParseConfig(connString)
	if err != nil {
		return nil, fmt.Errorf("parse connection string: %w", err)
	}
	if database != "" {
		cfg.Database = database
	}
	if host != "" {
		// The TLS server name follows Config.Host, so verify-full checks this
		// member's own certificate.
		cfg.Host = host
	}
	cfg.RuntimeParams["application_name"] = "pgbot"

	// Route the TCP leg through the SSH jump host when one is configured. This has
	// to happen before probe(): the probe connection dials too, and it must take
	// the same path as the pool. Installing it here rather than rewriting the DSN
	// to a local forward is what keeps sslmode= and .pgpass matching on the real
	// hostname — see sshtunnel.go.
	if dial := sshDialFunc(); dial != nil {
		// pgGo never resolves the host itself: DialFunc receives host:port, so
		// the SSH server resolves database hostnames.
		cfg.DialFunc = dial
	}

	// The driver never sends client-only params to the server. See clientOnlyParams.
	for _, p := range clientOnlyParams {
		if hasConnParam(connString, p) {
			fmt.Fprintf(os.Stderr, "pgbot: ignoring connection param %q — the driver can't honor it; TLS from sslmode still applies\n", p)
		}
	}

	// Probe capabilities + pooler on a throwaway connection first, so AfterConnect
	// applies only the GUCs this server understands. (Behind a transaction pooler
	// no protocol switch is needed: pgGo only uses the unnamed statement, parsed
	// and executed within one Sync, which poolers route as a unit.)
	caps, pooler, probePID, err := probe(ctx, cfg.Copy())
	if err != nil {
		return nil, err
	}

	// Track our own backend PIDs so collectors can exclude every pgbot connection
	// from pg_stat_activity, not just the one running a given query (ExcludeSelf).
	// The probe backend is gone by now but its PID still matters: `pgbot logs`
	// filters historical log lines by PID, and the probe wrote some.
	self := newSelfPIDs()
	self.add(probePID)
	pool := pggo.NewPool(pggo.PoolConfig{
		Config:          cfg,
		MaxConns:        maxConns,
		MaxConnLifetime: 5 * time.Minute,
		AfterConnect: func(ctx context.Context, c *pggo.Conn) error {
			if err := applySessionSetup(ctx, c, caps); err != nil {
				return err
			}
			self.add(c.PID())
			return nil
		},
		BeforeClose: func(c *pggo.Conn) {
			self.remove(c.PID())
		},
	})
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
	held := make([]*pggo.PoolConn, 0, maxConns)
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
func UnpinLocal(ctx context.Context, tx *pggo.Tx) error {
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
func applySessionSetup(ctx context.Context, c *pggo.Conn, caps Capabilities) error {
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
func probe(ctx context.Context, cc *pggo.Config) (Capabilities, PoolerInfo, uint32, error) {
	c, err := pggo.ConnectConfig(ctx, cc)
	if err != nil {
		return Capabilities{}, PoolerInfo{}, 0, fmt.Errorf("connect: %w", err)
	}
	defer c.Close()
	probePID := c.PID()

	// Detect the pooler first (named prepared statements, session persistence).
	pooler := detectPooler(ctx, c, cc)

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
		       -- Aurora exposes aurora_version() and aurora_replica_status(), but does
		       -- not catalogue them in pg_proc on every release (confirmed missing on
		       -- 18.3). Resolve them by signature rather than CALLING them: a call
		       -- fails on every other server, which writes an ERROR to the server log
		       -- and books a rollback in pg_stat_database on each pgbot run — the very
		       -- counter pgbot reports. to_regprocedure does neither.
		       to_regprocedure('aurora_version()') IS NOT NULL
		         OR to_regprocedure('aurora_replica_status()') IS NOT NULL`
	err = c.QueryRow(ctx, q).Scan(&caps.VersionNum, &caps.VersionText, &caps.Database,
		&caps.StartedAt, &caps.HasStatStatements, &caps.HasHypopg, &caps.HasPgMonitor,
		&mk.HasRDS, &mk.HasCloudSQL, &mk.HasAzure, &caps.InRecovery, &mk.IsAurora)
	if err != nil {
		return Capabilities{}, pooler, probePID, fmt.Errorf("probe capabilities: %w", err)
	}
	caps.RecoveryChecked = true // the probe scan succeeded, so InRecovery is trustworthy
	mk.Host, mk.VersionText = cc.Host, caps.VersionText
	caps.Provider = detectProvider(mk)

	// system_identifier makes the baseline fingerprint survive a restore/rename;
	// it needs pg_monitor/superuser on some providers, so it's best-effort.
	var sysID int64
	if err := c.QueryRow(ctx, `SELECT system_identifier FROM pg_control_system()`).Scan(&sysID); err == nil {
		caps.SystemIdentifier = fmt.Sprintf("%d", sysID)
	}

	// Installed extensions WITH the namespace each lives in. The schema is what
	// lets every extension read (pg_stat_statements, hypopg) address its objects
	// by qualified name — Supabase and friends install them in "extensions", off
	// the read-only role's search_path (issue #10). Best-effort: on failure the
	// map stays empty and callers fall back to bare names.
	if rows, err := c.Query(ctx, `SELECT e.extname, n.nspname FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace ORDER BY e.extname`); err == nil {
		type extRow struct {
			Name   string
			Schema string
		}
		if exts, err := pggo.CollectStructsByPos[extRow](rows); err == nil {
			caps.ExtensionSchemas = make(map[string]string, len(exts))
			for _, e := range exts {
				caps.Extensions = append(caps.Extensions, e.Name)
				caps.ExtensionSchemas[e.Name] = e.Schema
			}
		}
	}
	return caps, pooler, probePID, nil
}

// ReadOnlyTx runs fn inside its own short READ ONLY transaction and always rolls
// back. Each collector sample gets a fresh transaction — that, plus
// stats_fetch_consistency='none', is what keeps double-sampled rates non-zero.
func (t *Target) ReadOnlyTx(ctx context.Context, fn func(*pggo.Tx) error) error {
	tx, err := t.Pool.BeginTx(ctx, pggo.TxOptions{ReadOnly: true})
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
