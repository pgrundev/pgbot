package conn

import (
	"regexp"
	"strings"
)

var plainIdent = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// QuoteIdent double-quotes a PostgreSQL identifier unless it is a plain
// lowercase name, so generated SQL names exactly that role or database.
func QuoteIdent(s string) string {
	if plainIdent.MatchString(s) {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
