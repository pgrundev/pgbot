package why

import (
	"regexp"
	"testing"
	"unicode/utf8"
)

// referencesTableRegexp is the original implementation, kept as the oracle:
// referencesTable must agree with it on every input (#142).
func referencesTableRegexp(queryText, table string) bool {
	re := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(table) + `\b`)
	return re.MatchString(queryText)
}

var referencesTableCases = []struct {
	text, table string
}{
	{"SELECT * FROM orders WHERE id = $1", "orders"},
	{"select * from ORDERS where id = $1", "orders"},
	{"SELECT * FROM public.orders o", "orders"},
	{"SELECT * FROM order_items", "orders"},
	{"SELECT * FROM orders_archive", "orders"},
	{"SELECT * FROM preorders", "orders"},
	{"SELECT * FROM \"orders\"", "orders"},
	{"SELECT * FROM orders2", "orders"},
	{"orders", "orders"},
	{"", "orders"},
	{"SELECT 1", ""},
	{"", ""},
	{"!!!", ""},
	{"SELECT $1orders", "orders"},
	{"SELECT orders$1", "orders"},
	{"SELECT a.b FROM a.b", "a.b"},
	{"SELECT * FROM axb", "a.b"},
	{"SELECT * FROM ünïcode", "ünïcode"},
	{"SELECT * FROM xünïcode", "ünïcode"},
	{"SELECT * FROM ÜNÏCODE", "ünïcode"},
	{"SELECT * FROM Keys", "keys"}, // Kelvin sign folds to k
	{"SELECT * FROM keyſ", "keys"}, // long s folds to s
	{"SELECT * FROM \xffkeys", "keys"},
	{"SELECT * FROM t\xff", "t"},
	{"SELECT * FROM \xff", "�"},
	{"SELECT * FROM ordersorders", "orders"},
	{"SELECT * FROM ordersx orders", "orders"},
}

func TestReferencesTable_agreesWithRegexp(t *testing.T) {
	for _, c := range referencesTableCases {
		if got, want := referencesTable(c.text, c.table), referencesTableRegexp(c.text, c.table); got != want {
			t.Errorf("referencesTable(%q, %q) = %v, regexp says %v", c.text, c.table, got, want)
		}
	}
}

// referencedTables loops samples × tables for every shifted query, so the
// per-call matcher must not compile or allocate anything (#142).
func TestReferencesTable_doesNotAllocate(t *testing.T) {
	allocs := testing.AllocsPerRun(100, func() {
		for _, tbl := range benchTables {
			referencesTable(benchQuery, tbl)
		}
	})
	if allocs != 0 {
		t.Errorf("referencesTable allocated %.0f times per run, want 0", allocs)
	}
}

func FuzzReferencesTable(f *testing.F) {
	for _, c := range referencesTableCases {
		f.Add(c.text, c.table)
	}
	f.Fuzz(func(t *testing.T, text, table string) {
		if !utf8.ValidString(table) {
			t.Skip() // the regexp oracle panics on invalid UTF-8 patterns
		}
		if got, want := referencesTable(text, table), referencesTableRegexp(text, table); got != want {
			t.Errorf("referencesTable(%q, %q) = %v, regexp says %v", text, table, got, want)
		}
	})
}

const benchQuery = `SELECT o.id, o.total, c.email FROM public.orders o JOIN customers c ON c.id = o.customer_id ` +
	`LEFT JOIN order_items oi ON oi.order_id = o.id WHERE o.created_at > $1 AND c.region = $2 ORDER BY o.created_at DESC LIMIT $3`

var benchTables = []string{"orders", "customers", "order_items", "payments", "shipments", "audit_log", "users", "sessions"}

func BenchmarkReferencesTable(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		for _, tbl := range benchTables {
			referencesTable(benchQuery, tbl)
		}
	}
}

func BenchmarkReferencesTableRegexp(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		for _, tbl := range benchTables {
			referencesTableRegexp(benchQuery, tbl)
		}
	}
}
