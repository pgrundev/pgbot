package pglog

import (
	"encoding/csv"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// Parser turns a byte stream of one log format into Entries, incrementally:
// Parse may be called with arbitrary chunk boundaries; entries are emitted
// only once complete (their continuations can no longer grow), and Flush
// drains whatever is still pending at end of stream.
type Parser struct {
	format  Format
	carry   string // bytes after the last newline — an incomplete line
	pending *Entry // stderr: entry still absorbing continuation lines
	record  string // csv: lines of a record whose quotes haven't balanced yet
}

func NewParser(f Format) *Parser {
	return &Parser{format: f}
}

// Parse consumes the next chunk and returns every entry completed by it.
func (p *Parser) Parse(chunk []byte) []Entry {
	data := p.carry + string(chunk)
	var lines []string
	if i := strings.LastIndexByte(data, '\n'); i >= 0 {
		lines = strings.Split(data[:i], "\n")
		p.carry = data[i+1:]
	} else {
		p.carry = data
		return nil
	}

	var out []Entry
	for _, line := range lines {
		out = append(out, p.feedLine(line)...)
	}
	return out
}

// Flush returns the entry still pending (its continuations can no longer
// arrive). A partial carry line without a newline is NOT flushed — in follow
// mode it's simply not fully written yet.
func (p *Parser) Flush() []Entry {
	var out []Entry
	if p.format == FormatCSV && p.record != "" {
		if e, ok := parseCSVRecord(strings.TrimSuffix(p.record, "\n")); ok {
			out = append(out, e)
		}
		p.record = ""
	}
	if p.pending != nil {
		out = append(out, *p.pending)
		p.pending = nil
	}
	return out
}

// SkipPartial discards input up to the first line that begins a new entry —
// for a tail that starts at an arbitrary byte offset mid-file.
func (p *Parser) SkipPartial(data []byte) []byte {
	lines := strings.SplitAfter(string(data), "\n")
	for i, line := range lines {
		if p.startsEntry(strings.TrimSuffix(line, "\n")) {
			return []byte(strings.Join(lines[i:], ""))
		}
	}
	return nil
}

func (p *Parser) startsEntry(line string) bool {
	switch p.format {
	case FormatJSON:
		return strings.HasPrefix(line, "{")
	case FormatCSV:
		return reTimestampStart.MatchString(line) && p.record == ""
	default:
		m := reStderrLine.FindStringSubmatch(line)
		return m != nil && !isContinuationSeverity(m[2])
	}
}

func (p *Parser) feedLine(line string) []Entry {
	switch p.format {
	case FormatJSON:
		return p.feedJSON(line)
	case FormatCSV:
		return p.feedCSV(line)
	default:
		return p.feedStderr(line)
	}
}

// --- jsonlog: one JSON object per line, continuations are fields ---

type jsonLine struct {
	Timestamp string `json:"timestamp"`
	User      string `json:"user"`
	DBName    string `json:"dbname"`
	PID       int    `json:"pid"`
	Severity  string `json:"error_severity"`
	Message   string `json:"message"`
	Detail    string `json:"detail"`
	Hint      string `json:"hint"`
	Statement string `json:"statement"`
	Context   string `json:"context"`
	AppName   string `json:"application_name"`
}

func (p *Parser) feedJSON(line string) []Entry {
	if strings.TrimSpace(line) == "" {
		return nil
	}
	var j jsonLine
	if err := json.Unmarshal([]byte(line), &j); err != nil {
		return nil // torn or foreign line: skip rather than fabricate an entry
	}
	detail := joinNonEmpty(j.Detail, j.Hint, j.Statement, j.Context)
	return []Entry{{
		Time:     parsePgTime(j.Timestamp),
		Severity: j.Severity,
		Level:    levelFor(j.Severity, j.Message),
		Message:  j.Message,
		Detail:   detail,
		User:     j.User,
		Database: j.DBName,
		PID:      j.PID,
		AppName:  j.AppName,
	}}
}

// --- csvlog: records can span lines inside quoted fields; a record is
// complete once its quotes balance at a line end ---

var reTimestampStart = regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}`)

func (p *Parser) feedCSV(line string) []Entry {
	p.record += line + "\n"
	if quotesUnbalanced(p.record) {
		return nil
	}
	rec := strings.TrimSuffix(p.record, "\n")
	p.record = ""
	if e, ok := parseCSVRecord(rec); ok {
		return []Entry{e}
	}
	return nil
}

func quotesUnbalanced(s string) bool {
	return strings.Count(s, `"`)%2 == 1
}

// csvlog column order is fixed by PostgreSQL (docs: runtime-config-logging);
// later versions append columns, so indexing from the front is stable.
const (
	csvTime     = 0
	csvUser     = 1
	csvDatabase = 2
	csvPID      = 3
	csvSeverity = 11
	csvMessage  = 13
	csvDetail   = 14
	csvHint     = 15
	csvQuery    = 19
	csvAppName  = 22
)

func parseCSVRecord(rec string) (Entry, bool) {
	r := csv.NewReader(strings.NewReader(rec))
	r.FieldsPerRecord = -1
	fields, err := r.Read()
	if err != nil || len(fields) <= csvMessage {
		return Entry{}, false
	}
	get := func(i int) string {
		if i < len(fields) {
			return fields[i]
		}
		return ""
	}
	pid, _ := strconv.Atoi(get(csvPID))
	sev := get(csvSeverity)
	msg := get(csvMessage)
	return Entry{
		Time:     parsePgTime(get(csvTime)),
		Severity: sev,
		Level:    levelFor(sev, msg),
		Message:  msg,
		Detail:   joinNonEmpty(get(csvDetail), get(csvHint), get(csvQuery)),
		User:     get(csvUser),
		Database: get(csvDatabase),
		PID:      pid,
		AppName:  get(csvAppName),
	}, true
}

// --- stderr: prefix + SEVERITY:  message, continuations on following lines ---

// reStderrLine splits any prefixed line into (prefix, severity-word, rest).
// It tolerates arbitrary log_line_prefix content by keying on the severity
// token PostgreSQL always emits.
var reStderrLine = regexp.MustCompile(
	`^(.*?)\b(DEBUG[1-5]?|INFO|NOTICE|WARNING|ERROR|LOG|FATAL|PANIC|DETAIL|HINT|STATEMENT|CONTEXT|QUERY|LOCATION):\s{1,2}(.*)$`)

var (
	rePrefixTime   = regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}(?:\.\d{1,6})? [A-Za-z0-9+\-/_]+`)
	rePrefixPID    = regexp.MustCompile(`\[(\d+)\]`)
	rePrefixUserDB = regexp.MustCompile(`([A-Za-z0-9_$][\w$]*)@([A-Za-z0-9_$][\w$]*)`)
)

func isContinuationSeverity(s string) bool {
	switch s {
	case "DETAIL", "HINT", "STATEMENT", "CONTEXT", "QUERY", "LOCATION":
		return true
	}
	return false
}

func (p *Parser) feedStderr(line string) []Entry {
	m := reStderrLine.FindStringSubmatch(line)

	// No severity token: a raw continuation (multiline statement, tab-indented
	// autovacuum statistics). Attach to the pending entry, or skip noise.
	if m == nil {
		if p.pending != nil && line != "" {
			appendDetail(p.pending, strings.TrimSpace(line))
		}
		return nil
	}

	prefix, sev, rest := m[1], m[2], m[3]
	if isContinuationSeverity(sev) {
		if p.pending != nil {
			appendDetail(p.pending, rest)
		}
		return nil
	}

	e := Entry{
		Severity: sev,
		Level:    levelFor(sev, rest),
		Message:  rest,
	}
	if ts := rePrefixTime.FindString(prefix); ts != "" {
		e.Time = parsePgTime(ts)
	}
	if pm := rePrefixPID.FindStringSubmatch(prefix); pm != nil {
		e.PID, _ = strconv.Atoi(pm[1])
	}
	if um := rePrefixUserDB.FindStringSubmatch(prefix); um != nil {
		e.User, e.Database = um[1], um[2]
	}

	var out []Entry
	if p.pending != nil {
		out = append(out, *p.pending)
	}
	p.pending = &e
	return out
}

func appendDetail(e *Entry, s string) {
	if s == "" {
		return
	}
	if e.Detail == "" {
		e.Detail = s
		return
	}
	e.Detail += "\n" + s
}

func joinNonEmpty(parts ...string) string {
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "\n")
}
