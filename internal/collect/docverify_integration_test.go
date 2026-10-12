package collect_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pggo"
)

// TestDocVerifyQueries_run executes every "How to verify it yourself" SQL query
// from the findings catalogue against a real PostgreSQL fixture, read-only, and
// fails on any error. A wrong verify query is worse than none — the user runs it,
// gets a different answer, and concludes pgbot is wrong — and the structural test
// only checks that a ```sql block EXISTS, not that it runs. This catches typos,
// wrong catalog column names, and version-gated syntax automatically. It does NOT
// catch semantically-wrong-but-valid SQL (an unscoped filter that returns the
// wrong rows still runs), so the ~10 pages behind critical findings are also read
// by hand.
//
// Runs only on PostgreSQL 18+ (the newest catalog), so a modern view/column isn't
// flagged just because an older server lacks it; the one deprecated view whose
// columns moved (pg_stat_bgwriter) is tolerated explicitly.
func TestIntegration_docVerifyQueries(t *testing.T) {
	// Needs a SUPERUSER dsn — the fixture creates tables and an extension, which
	// the read-only pgbot_ro role of PGBOT_TEST_DSN can't. (The verify queries then
	// run read-only via conn.Connect, which pins the session regardless of role.)
	d := os.Getenv("PGBOT_TEST_SUPERUSER_DSN")
	if d == "" {
		t.Skip("set PGBOT_TEST_SUPERUSER_DSN (a superuser DSN) to run the doc-verify guard")
	}
	ctx := context.Background()

	admin, err := pggo.Connect(ctx, d)
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	defer admin.Close()

	var vnum int
	if err := admin.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").Scan(&vnum); err != nil {
		t.Fatal(err)
	}
	if vnum < 180000 {
		t.Skip("doc-verify runs on PG18+ only (avoids version-gated false positives on older catalogs)")
	}

	// Example objects the pages reference by name (public.issues.last_seen_at,
	// public.orders). Everything else is catalog views with literal filters.
	if _, err := admin.SimpleQuery(ctx, `
		CREATE EXTENSION IF NOT EXISTS pgstattuple;
		CREATE TABLE IF NOT EXISTS public.orders (id bigserial primary key, customer_id int, status int, amount numeric, note text);
		CREATE TABLE IF NOT EXISTS public.issues (id bigserial primary key, last_seen_at timestamptz, project_id int)`); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	defer admin.Exec(context.Background(), `DROP TABLE IF EXISTS public.orders, public.issues`)

	target, err := conn.Connect(ctx, d) // pinned read-only, exactly as a user would run these
	if err != nil {
		t.Fatalf("pgbot connect: %v", err)
	}
	defer target.Close()

	ran, skipped, versionGated := 0, 0, 0
	for _, b := range verifyBlocks(t) {
		if placeholderRE.MatchString(b.sql) {
			skipped++
			continue
		}
		for _, stmt := range splitStatements(b.sql) {
			ran++
			err := target.ReadOnlyTx(ctx, func(tx *pggo.Tx) error {
				_, e := tx.Exec(ctx, stmt)
				return e
			})
			if err == nil {
				continue
			}
			if tolerated(stmt, err) {
				versionGated++
				continue
			}
			t.Errorf("%s — verify query failed:\n    %s\n    %v", b.file, firstLine(stmt), err)
		}
	}
	t.Logf("doc-verify: ran %d statements, %d skipped (placeholder), %d tolerated (deprecated view)", ran, skipped, versionGated)
	if ran == 0 {
		t.Fatal("no verify queries ran — extraction is broken")
	}
}

type verifyBlock struct{ file, sql string }

var (
	verifySectionRE = regexp.MustCompile(`(?s)## How to verify it yourself\n(.*?)\n## `)
	sqlBlockRE      = regexp.MustCompile("(?s)```sql\n(.*?)```")
	placeholderRE   = regexp.MustCompile(`<\w+>|…|\.\.\.|your_|YOUR_|\bANALYZE\b`)
	lineCommentRE   = regexp.MustCompile(`--[^\n]*`)
)

// verifyBlocks returns every ```sql block inside a "How to verify it yourself"
// section across the catalogue.
func verifyBlocks(t *testing.T) []verifyBlock {
	t.Helper()
	dir := filepath.Join(docsRepoRoot(t), "docs", "findings")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []verifyBlock
	for _, e := range entries {
		n := e.Name()
		if !strings.HasSuffix(n, ".md") || strings.HasPrefix(n, "_") || n == "README.md" {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			t.Fatal(err)
		}
		sec := verifySectionRE.FindStringSubmatch(string(body))
		if sec == nil {
			continue
		}
		for _, m := range sqlBlockRE.FindAllStringSubmatch(sec[1], -1) {
			out = append(out, verifyBlock{file: n, sql: m[1]})
		}
	}
	return out
}

// splitStatements strips line comments and splits a block into individual
// statements (the extended protocol runs one at a time).
func splitStatements(block string) []string {
	block = lineCommentRE.ReplaceAllString(block, "")
	var out []string
	for _, s := range strings.Split(block, ";") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		// CREATE EXTENSION lines are setup the page tells the user to run once (e.g.
		// pgstattuple for exact bloat); they aren't read-only verify queries and
		// can't run in a read-only tx. The extension is present in the fixture, so
		// the measurement query that follows is still exercised.
		if strings.HasPrefix(strings.ToUpper(s), "CREATE EXTENSION") {
			continue
		}
		out = append(out, s)
	}
	return out
}

// tolerated reports whether an error is the known pg_stat_bgwriter column move
// (columns relocated to pg_stat_checkpointer in PG17) rather than a real defect.
func tolerated(stmt string, err error) bool {
	var pg *pggo.PgError
	if !errors.As(err, &pg) {
		return false
	}
	undefined := pg.Code == "42P01" || pg.Code == "42703" // undefined_table / undefined_column
	return undefined && strings.Contains(stmt, "pg_stat_bgwriter")
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i]) + " …"
	}
	return strings.TrimSpace(s)
}

// docsRepoRoot resolves the repo root from this test file (internal/collect/).
func docsRepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve caller path")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}
