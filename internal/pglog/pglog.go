// Package pglog parses PostgreSQL server logs (stderr, csvlog, jsonlog) into
// typed entries and tails them through a Source — over SQL (pg_read_file) or,
// later, a local file. Parsing is incremental: feed chunks as they arrive,
// completed entries come back, continuation lines (DETAIL, tab-indented
// statistics) attach to the entry they belong to.
package pglog

import (
	"path/filepath"
	"strings"
	"time"
)

// Format is the on-disk log format, per log_destination.
type Format int

const (
	FormatStderr Format = iota
	FormatCSV
	FormatJSON
)

// FormatForFile infers the format from the logfile's extension — how
// PostgreSQL itself distinguishes the three destinations' files.
func FormatForFile(name string) Format {
	switch filepath.Ext(name) {
	case ".json":
		return FormatJSON
	case ".csv":
		return FormatCSV
	default:
		return FormatStderr
	}
}

// Level is pgbot's four-way classification, one per filter toggle.
type Level string

const (
	LevelQuery Level = "query"
	LevelInfo  Level = "info"
	LevelWarn  Level = "warn"
	LevelError Level = "error"
	LevelAudit Level = "audit" // pgAudit trail lines (AUDIT: … at LOG severity)
)

// Entry is one parsed log event, whatever the source format.
type Entry struct {
	Time     time.Time `json:"time"`
	Severity string    `json:"severity"` // PostgreSQL's own: LOG, WARNING, ERROR, …
	Level    Level     `json:"level"`    // pgbot's: query, info, warn, error
	Message  string    `json:"message"`
	Detail   string    `json:"detail,omitempty"` // DETAIL/HINT/STATEMENT/CONTEXT continuations, joined
	User     string    `json:"user,omitempty"`
	Database string    `json:"database,omitempty"`
	PID      int       `json:"pid,omitempty"`
	AppName  string    `json:"app,omitempty"` // application_name where the format carries it (jsonlog, csvlog)
}

// levelFor maps a PostgreSQL severity (plus the message, which is what
// distinguishes a query line from any other LOG line) to a pgbot level.
func levelFor(severity, message string) Level {
	switch severity {
	case "ERROR", "FATAL", "PANIC":
		return LevelError
	case "WARNING":
		return LevelWarn
	}
	// pgAudit emits its trail at LOG severity with an AUDIT: prefix — its own
	// level, so the audit trail is filterable on its own.
	if strings.HasPrefix(message, "AUDIT: ") {
		return LevelAudit
	}
	// log_min_duration_statement / log_statement / extended-protocol lines all
	// arrive at LOG severity; the message shape is the query signal.
	if strings.HasPrefix(message, "duration: ") ||
		strings.HasPrefix(message, "statement: ") ||
		strings.HasPrefix(message, "execute ") {
		return LevelQuery
	}
	return LevelInfo
}

// pgTimeLayouts cover %m (milliseconds) and %t (seconds) timestamps.
var pgTimeLayouts = []string{
	"2006-01-02 15:04:05.000 MST",
	"2006-01-02 15:04:05 MST",
}

func parsePgTime(s string) time.Time {
	for _, l := range pgTimeLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
