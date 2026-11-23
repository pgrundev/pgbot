package monitor

import (
	"regexp"
	"strings"
	"unicode"

	"github.com/pgrundev/pgbot/internal/conn"
)

const (
	maxTitle = 64
	maxBody  = 220
)

var (
	// A connection URL anywhere in free text (error messages quote them).
	reURL = regexp.MustCompile(`postgres(?:ql)?://\S+`)
	// keyword form password=... (quoted or bare)
	reKwPassword = regexp.MustCompile(`(?i)(password\s*=\s*)('(?:[^'\\]|\\.)*'|\S+)`)
)

// Redact removes anything credential-shaped from text bound for a log line,
// the terminal, state.json or a notification: connection URLs lose their
// password (and are then reduced to host/db), keyword passwords are masked,
// and control characters are dropped.
func Redact(s string) string {
	s = reURL.ReplaceAllStringFunc(s, func(u string) string {
		return conn.RedactConnString(u)
	})
	s = reKwPassword.ReplaceAllString(s, "${1}REDACTED")
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

// clip shortens s to at most n runes on a word boundary, adding "…".
func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	cut := string(r[:n-1])
	if i := strings.LastIndexByte(cut, ' '); i > n/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ,.;:") + "…"
}
