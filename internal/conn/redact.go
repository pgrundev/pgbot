package conn

import (
	"net/url"
	"regexp"
	"strings"
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
// leave the machine via a query string. Order matters: strip comments first
// (free text no shape regex catches), then quoted regions (they may contain
// emails/uuids/numbers we'd otherwise leave a trace of), then the remaining bare
// identifiers-shaped-like-PII, then loose numbers.
func ScrubQueryText(sql string) string {
	if sql == "" {
		return sql
	}
	sql = scrubComments(sql)
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

// scrubComments replaces every comment body with "?" (`-- ?`, `/* ? */`). A lexer,
// not a regex: `--` inside a string isn't a comment, and `'` inside a comment
// isn't a string. An unterminated comment runs to the end — over-redacting.
func scrubComments(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	n := len(s)
	for i := 0; i < n; {
		c := s[i]
		switch {
		case c == '-' && i+1 < n && s[i+1] == '-':
			j := i + 2
			for j < n && s[j] != '\n' && s[j] != '\r' {
				j++
			}
			b.WriteString("-- ?")
			i = j
		case c == '/' && i+1 < n && s[i+1] == '*':
			depth, j := 1, i+2
			for j < n && depth > 0 {
				switch {
				case s[j] == '/' && j+1 < n && s[j+1] == '*':
					depth++
					j += 2
				case s[j] == '*' && j+1 < n && s[j+1] == '/':
					depth--
					j += 2
				default:
					j++
				}
			}
			b.WriteString("/* ? */")
			i = j
		case c == '\'':
			escapes := i > 0 && (s[i-1] == 'E' || s[i-1] == 'e') && (i < 2 || !isIdentChar(s[i-2]))
			j := i + 1
			for j < n {
				if escapes && s[j] == '\\' {
					j += 2
					continue
				}
				if s[j] == '\'' {
					if j+1 < n && s[j+1] == '\'' { // '' escapes a quote
						j += 2
						continue
					}
					j++
					break
				}
				j++
			}
			j = min(j, n)
			b.WriteString(s[i:j])
			i = j
		case c == '"':
			j := i + 1
			for j < n {
				if s[j] == '"' {
					if j+1 < n && s[j+1] == '"' {
						j += 2
						continue
					}
					j++
					break
				}
				j++
			}
			j = min(j, n)
			b.WriteString(s[i:j])
			i = j
		case c == '$' && (i == 0 || !isIdentChar(s[i-1])):
			if tag, ok := dollarTag(s[i:]); ok {
				end := strings.Index(s[i+len(tag):], tag)
				j := n
				if end >= 0 {
					j = i + len(tag) + end + len(tag)
				}
				b.WriteString(s[i:j])
				i = j
				continue
			}
			b.WriteByte(c)
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// dollarTag returns the $tag$ (or $$) opening s. `$1` is a parameter, not a quote.
func dollarTag(s string) (string, bool) {
	for j := 1; j < len(s); j++ {
		switch c := s[j]; {
		case c == '$':
			return s[:j+1], true
		case c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= 0x80:
		case c >= '0' && c <= '9' && j > 1:
		default:
			return "", false
		}
	}
	return "", false
}

func isIdentChar(c byte) bool {
	return c == '_' || c == '$' || c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= 0x80
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
