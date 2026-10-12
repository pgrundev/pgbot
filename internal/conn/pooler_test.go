package conn

import (
	"strings"
	"testing"

	"github.com/pgrundev/pggo"
)

func TestIsKnownPoolerEndpoint(t *testing.T) {
	cases := []struct {
		host string
		port uint16
		want bool
	}{
		{"db.abc.supabase.co", 6543, true},            // Supabase transaction pooler
		{"db.abc.supabase.co", 5432, false},           // Supabase direct
		{"ep-cool-name-pooler.neon.tech", 5432, true}, // Neon pooled
		{"ep-cool-name.neon.tech", 5432, false},       // Neon direct
		{"my-rds.amazonaws.com", 5432, false},
		{"127.0.0.1", 6432, false}, // generic PgBouncer on a nonstandard port — undetectable by signature
	}
	for _, c := range cases {
		cc := &pggo.Config{}
		cc.Host = c.host
		cc.Port = int(c.port)
		if got := isKnownPoolerEndpoint(cc); got != c.want {
			t.Errorf("isKnownPoolerEndpoint(%s:%d) = %v, want %v", c.host, c.port, got, c.want)
		}
	}
}

func TestPoolerHint(t *testing.T) {
	mk := func(host string, port int) *pggo.Config {
		cc := &pggo.Config{}
		cc.Host, cc.Port = host, port
		return cc
	}
	if h := poolerHint(mk("x-pooler.neon.tech", 5432)); !strings.Contains(h, "Neon") {
		t.Errorf("Neon hint wrong: %q", h)
	}
	if h := poolerHint(mk("x.supabase.co", 6543)); !strings.Contains(h, "Supabase") {
		t.Errorf("Supabase hint wrong: %q", h)
	}
	if h := poolerHint(mk("host", 5432)); !strings.Contains(h, "PgBouncer") {
		t.Errorf("generic hint wrong: %q", h)
	}
	// A "-pooler" host on a custom domain is NOT Neon (issue #22): PgDog and
	// self-hosted poolers reuse the naming convention. The endpoint still counts
	// as a pooler signal; only the label must stay generic.
	if h := poolerHint(mk("db-pooler.example.com", 5432)); strings.Contains(h, "Neon") {
		t.Errorf("custom -pooler host must not be labeled Neon: %q", h)
	}
	if h := poolerHint(mk("db-pooler.example.com", 5432)); !strings.Contains(h, "PgBouncer") {
		t.Errorf("custom -pooler host should get the generic hint: %q", h)
	}
}

func TestPgDogVerdict(t *testing.T) {
	const want = "pgbot_42"
	nonce := want
	stale := "pgbot_old"
	zero := "0"
	empty := ""
	cases := []struct {
		name           string
		control, shard *string
		verdict        bool
	}{
		// PgDog intercepts session-level SET pgdog.* and never forwards it: the
		// control echoes, the shard never arrived on a fresh backend.
		{"consumed, fresh backend (PgDog)", &nonce, nil, true},
		// Same, but the pooled server connection carries a leftover empty
		// placeholder from another client — still not the value we sent.
		{"consumed, stale placeholder (PgDog)", &nonce, &empty, true},
		// Real Postgres, or a forwarding pooler serving both round trips from
		// one backend: both placeholders echo exactly.
		{"forwarded (direct or PgBouncer)", &nonce, &zero, false},
		// Backend switched between SET and readback: the control vanished too,
		// so a missing shard proves nothing — inconclusive, not PgDog.
		{"backend switch (inconclusive)", nil, nil, false},
		{"stale control (inconclusive)", &stale, nil, false},
	}
	for _, c := range cases {
		if got := pgdogVerdict(c.control, c.shard, want); got != c.verdict {
			t.Errorf("%s: pgdogVerdict = %v, want %v", c.name, got, c.verdict)
		}
	}
}

func TestPoolerMessages(t *testing.T) {
	p := PoolerInfo{Hint: "the Supabase transaction pooler (port 6543)"}
	if !strings.Contains(p.Note(), "rates are still correct") {
		t.Error("Note should reassure that rates are correct")
	}
	if !strings.Contains(p.Note(), "--strict-pooler") {
		t.Error("Note should mention the strict flag")
	}
	if !strings.Contains(p.StrictMessage(), "port 5432, not 6543") {
		t.Error("StrictMessage should give the fix")
	}
}
