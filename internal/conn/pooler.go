package conn

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"

	"github.com/jackc/pgx/v5"
)

// PoolerInfo records whether the connection routes through a transaction pooler
// (PgBouncer transaction mode, Supabase :6543, Neon -pooler). Behind one,
// session-level SET does not persist between statements — so statement_timeout,
// default_transaction_read_only and especially stats_fetch_consistency='none'
// all silently evaporate, which reintroduces the stats-caching bug and yields
// zeroed rates that LOOK like a healthy run. We must detect this, not guess it.
type PoolerInfo struct {
	Detected       bool
	SimpleProtocol bool   // prepared statements failed; use the simple protocol
	Hint           string // provider-specific fix, for the error message
}

// detectPooler runs two probes on an already-open connection:
//
//  1. Prepared-statement support — a transaction pooler that predates
//     PgBouncer's prepared-statement support rejects Parse/Bind. Failure means
//     we must fall back to the simple protocol for every later query.
//  2. Session-setting persistence (the PRIMARY signal) — SET application_name to
//     a nonce in one autocommit statement, then read it back in a SEPARATE round
//     trip. On a direct connection the value persists; behind a transaction
//     pooler the two statements may land on different backends, so it doesn't.
//
// Host/port heuristics are only used to make the error message friendlier.
func detectPooler(ctx context.Context, c *pgx.Conn, cc *pgx.ConnConfig) PoolerInfo {
	return detectPoolerForEngine(ctx, c, cc, EnginePostgreSQL)
}

func detectPoolerForEngine(ctx context.Context, c *pgx.Conn, cc *pgx.ConnConfig, engine Engine) PoolerInfo {
	info := PoolerInfo{Hint: poolerHint(cc)}

	// (1) prepared-statement probe
	psName := fmt.Sprintf("pgbot_ps_%d", nonce())
	if _, err := c.Prepare(ctx, psName, "SELECT 1"); err != nil {
		info.SimpleProtocol = true
	} else {
		_ = c.Deallocate(ctx, psName)
	}

	// (2) session-persistence probe, forced through the simple protocol so a
	// prepared-statement failure doesn't masquerade as non-persistence.
	want := fmt.Sprintf("pgbot_probe_%d", nonce())
	if _, err := c.Exec(ctx, "SET application_name = '"+want+"'"); err == nil {
		var got string
		err := c.QueryRow(ctx, "SELECT current_setting('application_name')", pgx.QueryExecModeSimpleProtocol).Scan(&got)
		if err == nil && got != want {
			info.Detected = true
		}
	}
	_, _ = c.Exec(ctx, "SET application_name = 'pgbot'") // restore

	// A prepared-statement failure is itself a strong transaction-pooler signal.
	// So are the well-known managed-pooler endpoints — a single idle client gets
	// a stable backend through PgBouncer, so the persistence probe alone cannot
	// catch modern transaction poolers; the host/port signature is reliable for
	// the ones users actually hit (Supabase :6543, Neon -pooler).
	if info.SimpleProtocol || isKnownPoolerEndpoint(cc) {
		info.Detected = true
	}

	// (3) PgDog identification (issue #22) — behavioral, never hostname-based.
	// PgDog consumes SET pgdog.* routing hints instead of forwarding them, so a
	// SET LOCAL that vanishes within its own transaction can only mean PgDog.
	if engine == EnginePostgreSQL && detectPgDog(ctx, c) {
		info.Detected = true
		info.Hint = "a PgDog pooler"
	}
	return info
}

// detectPgDog probes for PgDog by setting the pgdog.shard routing hint and
// reading it back. PgDog intercepts session-level SET pgdog.* and does not
// forward it, so the backend never sees the value; on real PostgreSQL the
// dotted name is stored as a placeholder GUC and echoes back. A control GUC
// (pgbot.probe) set the same way rules out the transaction-pooler false
// positive: if PgBouncer split the two round trips across backends, the control
// vanishes too and the probe reads inconclusive, not PgDog.
//
// Verified against PgDog v0.1.54: only SESSION-level SET is intercepted (SET
// LOCAL inside an explicit transaction is forwarded verbatim), and shard 0
// exists in every deployment. pgdog.sharding_key must never be used here — on
// some configs it errors and poisons the session for every later statement.
func detectPgDog(ctx context.Context, c *pgx.Conn) bool {
	want := fmt.Sprintf("pgbot_%d", nonce())
	if _, err := c.Exec(ctx, "SET pgbot.probe = '"+want+"'"); err != nil {
		return false
	}
	if _, err := c.Exec(ctx, "SET pgdog.shard = 0"); err != nil {
		_, _ = c.Exec(ctx, "SET pgbot.probe TO DEFAULT")
		return false
	}
	var control, shard *string
	err := c.QueryRow(ctx,
		"SELECT current_setting('pgbot.probe', true), current_setting('pgdog.shard', true)",
		pgx.QueryExecModeSimpleProtocol).Scan(&control, &shard)
	// Clean up with SET … TO DEFAULT, never RESET: SET of a dotted name is
	// accepted even by a backend that never saw the parameter, while RESET
	// errors there — and pgbot must not book server errors it would then report.
	_, _ = c.Exec(ctx, "SET pgdog.shard TO DEFAULT")
	_, _ = c.Exec(ctx, "SET pgbot.probe TO DEFAULT")
	return err == nil && pgdogVerdict(control, shard, want)
}

// pgdogVerdict decides from the two readbacks. The control must echo the nonce
// — otherwise the SETs demonstrably didn't reach the backend that answered
// (transaction-pooler backend switch) and the probe is inconclusive. With the
// control confirmed, anything but the exact value we set means pgdog.shard was
// intercepted en route: NULL on a fresh backend, or a leftover empty
// placeholder on a pooled server connection another client touched.
func pgdogVerdict(control, shard *string, want string) bool {
	if control == nil || *control != want {
		return false
	}
	return shard == nil || *shard != "0"
}

func isKnownPoolerEndpoint(cc *pgx.ConnConfig) bool {
	return strings.Contains(strings.ToLower(cc.Host), "-pooler") || cc.Port == 6543
}

func poolerHint(cc *pgx.ConnConfig) string {
	host := strings.ToLower(cc.Host)
	switch {
	// "-pooler" alone is not Neon (issue #22): PgDog and self-hosted poolers
	// reuse the naming convention. Only Neon's own domain earns the Neon label.
	case strings.Contains(host, "-pooler") && strings.Contains(host, "neon.tech"):
		return "a Neon pooled endpoint"
	case cc.Port == 6543:
		return "the Supabase transaction pooler (port 6543)"
	default:
		return "a transaction pooler (PgBouncer-style)"
	}
}

// Note is the one-line message printed when a pooler is detected and pgbot
// proceeds (the default). Rates are still correct because pgbot reads each
// counter in its own transaction — pg_stat_* are cluster-wide, so the backend
// that serves a read doesn't matter.
func (p PoolerInfo) Note() string {
	return fmt.Sprintf("pgbot: connected via %s — proceeding; rates are still correct "+
		"(each counter is read in its own transaction). Pass --strict-pooler to refuse instead.", p.Hint)
}

// StrictMessage is the exit-3 guidance shown under --strict-pooler.
func (p PoolerInfo) StrictMessage() string {
	return fmt.Sprintf(`Connected through %s, and --strict-pooler is set.

Use the direct connection endpoint instead:
  Neon      → the host without "-pooler"
  Supabase  → port 5432, not 6543
  PgDog     → connect to Postgres directly
  PgBouncer → session mode, or connect to Postgres directly`, p.Hint)
}

func nonce() int64 {
	n, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		return 1 // detection still works; the nonce only needs to be unlikely to collide
	}
	return n.Int64()
}
