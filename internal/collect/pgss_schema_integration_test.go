package collect_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/pgrundev/pgbot/internal/collect"
	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/model"
	"github.com/pgrundev/pggo"
)

// Issue #10: pg_stat_statements installed outside public (Supabase's
// `extensions` schema is the common case) was detected but unreadable — the
// capability list said "pg_stat_statements" while the queries section came back
// `unavailable: relation "pg_stat_statements" does not exist`. This relocates the
// extension (it is relocatable) to a schema that is NOT on anyone's search_path,
// runs the collectors as the read-only role, and requires the queries section to
// be fully populated. Needs the superuser DSN to move the extension; the read
// path runs as PGBOT_TEST_DSN exactly as a user would.
func TestIntegration_pgssInNonDefaultSchema(t *testing.T) {
	su := os.Getenv("PGBOT_TEST_SUPERUSER_DSN")
	if su == "" {
		t.Skip("set PGBOT_TEST_SUPERUSER_DSN (a superuser DSN) to run the relocated-extension test")
	}
	ro := dsn(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	admin, err := pggo.Connect(ctx, su)
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	// Registered before the restore cleanup below: cleanups run last-in-first-out,
	// so the extension is moved back on a still-open connection.
	t.Cleanup(func() { admin.Close() })

	var installed bool
	if err := admin.QueryRow(ctx, `SELECT count(*) > 0 FROM pg_extension WHERE extname = 'pg_stat_statements'`).Scan(&installed); err != nil {
		t.Fatal(err)
	}
	if !installed {
		t.Skip("pg_stat_statements is not installed on this server")
	}
	var origSchema string
	if err := admin.QueryRow(ctx, `SELECT n.nspname FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace WHERE e.extname = 'pg_stat_statements'`).Scan(&origSchema); err != nil {
		t.Fatal(err)
	}

	// Move it. USAGE is granted to PUBLIC (as Supabase does for `extensions`), so
	// the read-only role can see the objects — the point is that they are found
	// by *qualified* name, not via search_path.
	const relocated = "pgbot_ext_test"
	if _, err := admin.SimpleQuery(ctx, `CREATE SCHEMA IF NOT EXISTS `+relocated+`; GRANT USAGE ON SCHEMA `+relocated+` TO PUBLIC; ALTER EXTENSION pg_stat_statements SET SCHEMA `+relocated); err != nil {
		t.Fatalf("relocate extension: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		if _, err := admin.SimpleQuery(c, `ALTER EXTENSION pg_stat_statements SET SCHEMA `+pggo.QuoteIdentifier(origSchema)+`; DROP SCHEMA IF EXISTS `+relocated); err != nil {
			t.Errorf("restore extension schema: %v", err)
		}
	})
	// The relocated view must be readable by the RO role in the way a Supabase
	// dedicated read-only role would be (pg_monitor grants pg_read_all_stats, which
	// pg_stat_statements checks; SELECT on the view itself is the ordinary grant).
	if _, err := admin.Exec(ctx, `GRANT SELECT ON ALL TABLES IN SCHEMA `+relocated+` TO PUBLIC`); err != nil {
		t.Fatalf("grant: %v", err)
	}
	// Something for the top list to contain.
	if _, err := admin.Exec(ctx, `SELECT count(*) FROM pg_class`); err != nil {
		t.Fatal(err)
	}

	target, err := conn.Connect(ctx, ro)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer target.Close()
	if !target.Caps.HasStatStatements {
		t.Fatalf("probe must still detect pg_stat_statements after relocation; caps=%+v", target.Caps)
	}
	if got := target.Caps.ExtensionSchema("pg_stat_statements"); got != relocated {
		t.Fatalf("probe must discover the extension schema: got %q want %q", got, relocated)
	}

	c, err := collect.Run(ctx, target, collect.Options{Interval: 200 * time.Millisecond, ASHHz: 0})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if c.Queries == nil {
		t.Fatal("queries section missing")
	}
	if c.Queries.Exactness != model.ExactnessCumulative {
		t.Fatalf("queries section must be readable with the extension in %q: exactness=%q reason=%q", relocated, c.Queries.Exactness, c.Queries.Reason)
	}
	if !c.Queries.Enabled || len(c.Queries.Top) == 0 {
		t.Fatalf("expected a populated top-queries list, got enabled=%v top=%d", c.Queries.Enabled, len(c.Queries.Top))
	}
	// PgssMax / count come from the (also qualified) SRF and setting reads.
	if c.Queries.PgssCount == 0 || c.Queries.PgssMax == 0 {
		t.Errorf("pgss saturation health must be read via the qualified SRF: count=%d max=%d", c.Queries.PgssCount, c.Queries.PgssMax)
	}
	if target.Caps.VersionNum >= 140000 && c.Queries.Reason != "" {
		t.Errorf("unexpected reason on a healthy relocated pgss: %q", c.Queries.Reason)
	}
}
