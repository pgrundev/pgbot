package conn

import (
	"net/url"
	"regexp"
)

// Privacy is enforced here, in code — not in docs. pg_stat_activity.query carries
// raw SQL including literal values (a PII vector). pg_stat_statements normalizes
// DML parameters to $1 — BUT it stores UTILITY statements VERBATIM: CREATE USER …
// PASSWORD 'secret', ALTER ROLE … PASSWORD, COPY … FROM PROGRAM '…', DO $$…$$ all
// keep their literals. So "pgss text is safe" is FALSE; both pg_stat_activity and
// pg_stat_statements text pass through ScrubQueryText before entering a Context.
// (ScrubQueryText preserves $N placeholders so normalized DML is unharmed.)

var (
	// '...' string literals (SQL-escaped ''); E'...' also takes backslash escapes.
	reSingleQuoted = regexp.MustCompile(`\b[Ee]'(?:[^'\\]|\\.|'')*'|'(?:[^']|'')*'`)
	// Dollar-quoted regions. RE2 has no backreferences, so we match an opening
	// $tag$ to the NEXT $tag$ non-greedily rather than the same tag — for
	// well-formed bodies this is exact, and for odd input it over-redacts, which
	// is the safe direction for a privacy scrubber.
	reDollarQuoted = regexp.MustCompile(`\$[A-Za-z0-9_]*\$[\s\S]*?\$[A-Za-z0-9_]*\$`)
	reEmail        = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	reUUID         = regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`)
	reNumber       = regexp.MustCompile(`\b\d+(?:\.\d+)?\b`)
	// A $N placeholder (from pg_stat_statements normalization) OR a bare numeric
	// literal. The alternation matches $N first, so the replace func sees it whole
	// and preserves it — we scrub bare literals but must keep normalized
	// placeholders intact, because ScrubQueryText now also runs over pgss text
	// (utility statements like DO blocks are stored VERBATIM by pgss, not
	// normalized, so they can carry real literals).
	reNumberOrPlaceholder = regexp.MustCompile(`\$\d+|\b\d+(?:\.\d+)?\b`)

	// Connection-string passwords: `?password=…` in a URL's query (libpq accepts
	// it there as well as in the userinfo) and `password=…` in keyword form.
	reURLQueryPassword = regexp.MustCompile(`(?i)(^|&)password=[^&]*`)
	reKeywordPassword  = regexp.MustCompile(`(?i)(password\s*=\s*)('[^']*'|"[^"]*"|\S+)`)
)

// ScrubQueryText removes literal values from raw SQL so no customer data can
// leave the machine via a query string. Order matters: strip quoted regions
// first (they may contain emails/uuids/numbers we'd otherwise leave a trace of),
// then the remaining bare identifiers-shaped-like-PII, then loose numbers.
func ScrubQueryText(sql string) string {
	if sql == "" {
		return sql
	}
	// Literal replacement, NOT ReplaceAllString: the latter applies Expand
	// semantics, so "$REDACTED$" would be parsed as a reference to a (nonexistent)
	// capture group named "REDACTED" plus a trailing "$", both expanding to empty
	// — deleting the marker entirely (over-redacts safely, but a reader sees
	// "DO ;" with no signal that content was removed). Use literal replacement
	// everywhere so a "$" in any replacement is never re-interpreted.
	s := reDollarQuoted.ReplaceAllLiteralString(sql, "$REDACTED$")
	s = reSingleQuoted.ReplaceAllLiteralString(s, "'?'")
	s = reEmail.ReplaceAllLiteralString(s, "?")
	s = reUUID.ReplaceAllLiteralString(s, "?")
	s = reNumberOrPlaceholder.ReplaceAllStringFunc(s, func(m string) string {
		if m[0] == '$' { // keep $N pg_stat_statements placeholders
			return m
		}
		return "?"
	})
	return s
}

// RedactConnString returns a connection string safe to print in logs, errors,
// and JSON: the password is replaced with "REDACTED". Accepts URL form
// (postgres://user:pass@host/db, or ?password=… in the query) and best-effort
// keyword form (password=...).
func RedactConnString(cs string) string {
	if cs == "" {
		return cs
	}
	if u, err := url.Parse(cs); err == nil && (u.Scheme == "postgres" || u.Scheme == "postgresql") {
		if _, hasPw := u.User.Password(); hasPw {
			u.User = url.UserPassword(u.User.Username(), "REDACTED")
		}
		u.RawQuery = reURLQueryPassword.ReplaceAllString(u.RawQuery, "${1}password=REDACTED")
		return u.String()
	}
	// keyword/DSN form: password=secret or password='secret'
	return reKeywordPassword.ReplaceAllString(cs, `${1}REDACTED`)
}
