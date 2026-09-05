package conn

import (
	"sort"
	"strconv"
	"strings"
	"sync"
)

// selfPIDs is the live set of backend PIDs for pgbot's own pool connections,
// maintained by the pool's AfterConnect (add) and BeforeClose (remove) hooks. It
// always reflects the connections currently open, so a closed connection's PID
// is dropped before Postgres can reuse it for an unrelated backend.
type selfPIDs struct {
	mu sync.Mutex
	m  map[uint32]struct{}
}

func newSelfPIDs() *selfPIDs { return &selfPIDs{m: make(map[uint32]struct{})} }

func (s *selfPIDs) add(pid uint32) {
	if pid == 0 {
		return
	}
	s.mu.Lock()
	s.m[pid] = struct{}{}
	s.mu.Unlock()
}

func (s *selfPIDs) remove(pid uint32) {
	s.mu.Lock()
	delete(s.m, pid)
	s.mu.Unlock()
}

func (s *selfPIDs) list() []uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]uint32, 0, len(s.m))
	for p := range s.m {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// selfFilter is the literal every pg_stat_activity query carries to drop the
// querying backend. ExcludeSelf rewrites it in place.
const selfFilter = "pid <> pg_backend_pid()"

// SelfPIDs returns the backend PIDs of every connection pgbot has opened to
// this server (probe included), so `pgbot logs` can drop its own log lines
// the way collectors drop their own sessions.
func (t *Target) SelfPIDs() []uint32 { return t.self.list() }

// ExcludeSelf rewrites a pg_stat_activity query so it ignores EVERY one of
// pgbot's own pool connections — not just the backend running the query.
//
// pgbot samples through a small pool; between short READ ONLY samples each
// connection sits briefly idle-in-transaction and holds a backend_xmin. Counting
// its own backends turns the monitor into the finding: phantom
// idle-in-transaction sessions, a self-pinned vacuum horizon, connection-
// saturation slots pgbot is itself consuming, wait-profile noise. Every query
// that reads pg_stat_activity for user activity passes its SQL through here.
// (This is the same principle as ReadOnlyTx committing rather than rolling back,
// so pgbot doesn't inflate the rollback ratio it reports.)
//
// Exclusion is by backend PID, captured when the pool warms: unspoofable — a
// session cannot hide by setting application_name='pgbot' — and collision-free —
// a user service legitimately named 'pgbot' is still monitored. PIDs come from
// the driver, never user input, so they are inlined into the SQL; that also keeps
// the filter working on the simple-protocol path used behind transaction poolers,
// where array bind parameters are awkward.
func (t *Target) ExcludeSelf(sql string) string {
	return strings.ReplaceAll(sql, selfFilter, t.selfPredicate())
}

// selfPredicate returns the SQL that excludes pgbot's own backends. With an empty
// set (e.g. before the pool warms) it degrades to the old pg_backend_pid()-only
// filter — a safe subset that never over-excludes.
func (t *Target) selfPredicate() string {
	if t == nil || t.self == nil {
		return selfFilter
	}
	pids := t.self.list()
	if len(pids) == 0 {
		return selfFilter
	}
	var b strings.Builder
	b.WriteString(selfFilter)
	b.WriteString(" AND pid <> ALL(ARRAY[")
	for i, p := range pids {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatUint(uint64(p), 10))
	}
	b.WriteString("]::int[])")
	return b.String()
}
