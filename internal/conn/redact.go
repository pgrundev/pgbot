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
	reSingleQuoted = regexp.MustCompile(`'(?:[^']|'')*'`) // '...' string literals (SQL-escaped '')
	// Dollar-quoted regions. RE2 has no backreferences, so we match an opening
	// $tag$ to the NEXT $tag$ non-greedily rather than the same tag — for
	// well-formed bodies this is exact, and for odd input it over-redacts, which
	// is the safe direction for a privacy scrubber.
	reDollarQuoted = regexp.MustCompile(`\$[A-Za-z0-9_]*\$[\s\S]*?\$[A-Za-z0-9_]*\$`)
	reEmail        = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	reUUID         = regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`)
	reNumber       = regexp.MustCompile(`\b\d+(?:\.\d+)?\b`)
	// Diagnostic strings are not necessarily valid SQL. CockroachDB job errors,
	// for example, can contain pretty-printed range keys in double quotes or an
	// external-storage URI outside a SQL literal. These patterns are deliberately
	// broader than ScrubQueryText: losing some detail is preferable to retaining
	// a row key, credential, or sink URI in JSON.
	reDiagnosticDoubleQuoted = regexp.MustCompile(`"(?:[^"]|"")*"`)
	reDiagnosticURI          = regexp.MustCompile(`(?i)\b(?:https?|s3|gs|azure|nodelocal|external|kafka|webhook)://[^\s,;]+`)
	reDiagnosticSecret       = regexp.MustCompile(`(?i)\b(?:access[_-]?key|secret|token|password|credential|signature|sig)\s*=\s*[^\s,;]+`)
	reDiagnosticLongHex      = regexp.MustCompile(`\b[0-9a-fA-F]{16,}\b`)
	// CockroachDB redactable strings retain unsafe values between these markers;
	// the markers describe what a redactor must remove, not text already removed.
	reCRDBUnsafe = regexp.MustCompile(`‹[^›]*›`)
	// A $N placeholder (from pg_stat_statements normalization) OR a bare numeric
	// literal. The alternation matches $N first, so the replace func sees it whole
	// and preserves it — we scrub bare literals but must keep normalized
	// placeholders intact, because ScrubQueryText now also runs over pgss text
	// (utility statements like DO blocks are stored VERBATIM by pgss, not
	// normalized, so they can carry real literals).
	reNumberOrPlaceholder = regexp.MustCompile(`\$\d+|\b\d+(?:\.\d+)?\b`)
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

// ScrubRedactableText removes CockroachDB's marked unsafe fragments, then
// applies the normal query scrubber to any remaining SQL-shaped literals.
func ScrubRedactableText(s string) string {
	s = reCRDBUnsafe.ReplaceAllLiteralString(s, "?")
	return ScrubQueryText(s)
}

// ScrubDiagnosticText removes values from free-form diagnostic messages. It is
// stricter than ScrubQueryText because error/status text can embed range keys,
// object values, or external-service URIs without SQL literal syntax.
func ScrubDiagnosticText(s string) string {
	s = reDiagnosticURI.ReplaceAllLiteralString(s, "<redacted-uri>")
	s = reDiagnosticSecret.ReplaceAllLiteralString(s, "REDACTED")
	s = reDiagnosticDoubleQuoted.ReplaceAllLiteralString(s, `"?"`)
	s = reDiagnosticLongHex.ReplaceAllLiteralString(s, "?")
	return ScrubRedactableText(s)
}

// RedactConnString returns a connection string safe to print in logs, errors,
// and JSON: the password is replaced with "***". Accepts URL form
// (postgres://user:pass@host/db) and best-effort keyword form (password=...).
func RedactConnString(cs string) string {
	if cs == "" {
		return cs
	}
	if u, err := url.Parse(cs); err == nil && u.Scheme != "" && u.User != nil {
		if _, hasPw := u.User.Password(); hasPw {
			u.User = url.UserPassword(u.User.Username(), "REDACTED")
		}
		return u.String()
	}
	// keyword/DSN form: password=secret or password='secret'
	out := regexp.MustCompile(`(?i)(password\s*=\s*)('[^']*'|"[^"]*"|\S+)`).
		ReplaceAllString(cs, `${1}REDACTED`)
	return out
}
