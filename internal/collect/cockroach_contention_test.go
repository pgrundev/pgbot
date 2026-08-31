package collect

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/model"
)

func TestAssembleCockroachContention(t *testing.T) {
	now := time.Date(2026, 8, 28, 5, 30, 0, 0, time.UTC)
	d := &model.CockroachDB{}
	assembleCockroachContention(d, conn.Capabilities{HasCRDBContention: true}, cockroachSample{
		Contention: []cockroachContentionRow{{
			WaitingStmtFingerprint: "waiter", BlockingTxnFingerprint: "0000000000000000",
			Database: "app", Schema: "public", Table: "accounts", Index: "accounts_pkey", Type: "LOCK_WAIT",
			Events: 12, TotalSeconds: 8.5, MaxSeconds: 2.25, LastSeen: now,
			TotalEvents: 17, TotalSecondsAll: 10, MaxSecondsAll: 3, SerializationConflicts: 5,
		}},
	})
	h := d.Contention
	if h.Exactness != model.ExactnessScraped || h.WindowMinutes != 60 || !h.Bounded || h.TotalEvents != 17 || h.TotalWaitMS != 10_000 || h.MaxWaitMS != 3_000 {
		t.Fatalf("contention=%+v", h)
	}
	if len(h.Hotspots) != 1 || h.Hotspots[0].BlockingTxnFingerprint != "" || h.Hotspots[0].TotalWaitMS != 8_500 {
		t.Fatalf("hotspots=%+v", h.Hotspots)
	}
	if h.Hotspots[0].WaiterResolution != model.CockroachContentionNotFound ||
		h.Hotspots[0].BlockerResolution != model.CockroachContentionNotResolved {
		t.Fatalf("resolution states=%+v", h.Hotspots[0])
	}
}

func TestAssembleCockroachContentionAttribution(t *testing.T) {
	d := &model.CockroachDB{}
	assembleCockroachContention(d, conn.Capabilities{HasCRDBContention: true}, cockroachSample{
		Contention: []cockroachContentionRow{{
			WaitingStmtFingerprint: "waiter", BlockingTxnFingerprint: "blocker",
			Database: "app", Schema: "public", Table: "accounts", Type: "LOCK_WAIT",
		}},
		ContentionAttribution: []cockroachContentionAttributionRow{
			{Kind: "statement", Fingerprint: "waiter", Query: "UPDATE accounts SET email = 'alice@example.com'", AppNames: []string{"api", "api"}},
			{Kind: "transaction", Fingerprint: "stmt-1", TransactionFingerprint: "blocker", Query: "SELECT * FROM accounts WHERE id = 42", AppNames: []string{"worker"}},
			{Kind: "transaction", Fingerprint: "stmt-2", TransactionFingerprint: "blocker", Query: "UPDATE accounts SET balance = 99", AppNames: []string{"worker"}},
		},
	})
	h := d.Contention.Hotspots[0]
	if h.WaiterResolution != model.CockroachContentionResolved || h.BlockerResolution != model.CockroachContentionResolved {
		t.Fatalf("resolution states=%+v", h)
	}
	if len(h.WaitingApplications) != 1 || h.WaitingApplications[0] != "api" || len(h.BlockingQueries) != 2 || len(h.BlockingApplications) != 1 {
		t.Fatalf("attribution=%+v", h)
	}
	for _, secret := range []string{"alice@example.com", "42", "99"} {
		if strings.Contains(h.WaitingQuery+strings.Join(h.BlockingQueries, " "), secret) {
			t.Fatalf("query attribution leaked literal %q: %+v", secret, h)
		}
	}
}

func TestAssembleCockroachContentionAttributionUnavailable(t *testing.T) {
	d := &model.CockroachDB{}
	assembleCockroachContention(d, conn.Capabilities{HasCRDBContention: true}, cockroachSample{
		Contention:               []cockroachContentionRow{{WaitingStmtFingerprint: "waiter", BlockingTxnFingerprint: "blocker"}},
		ContentionAttributionErr: errors.New("statistics timed out"),
	})
	h := d.Contention.Hotspots[0]
	if h.WaiterResolution != model.CockroachContentionStatsUnavailable || h.BlockerResolution != model.CockroachContentionStatsUnavailable {
		t.Fatalf("resolution states=%+v", h)
	}
}

func TestCockroachContentionAttributionQueriesStayRedactedAndBounded(t *testing.T) {
	for name, query := range map[string]string{
		"current": sqlCockroachContentionAttribution,
		"legacy":  sqlCockroachContentionAttributionLegacy,
	} {
		for _, forbidden := range []string{"contending_key", "contending_pretty_key", "blocking_txn_id", "waiting_txn_id"} {
			if strings.Contains(query, forbidden) {
				t.Errorf("%s attribution query selects %s", name, forbidden)
			}
		}
		for _, required := range []string{"statement_rank <= 5", "fingerprint_id IN (%s)", "24 hours"} {
			if !strings.Contains(query, required) {
				t.Errorf("%s attribution query missing %q", name, required)
			}
		}
	}
}

func TestEnrichCockroachContentionFromQueryStats(t *testing.T) {
	c := &model.Context{
		Queries: &model.Queries{Top: []model.QueryStat{{Fingerprint: "waiter", AppName: "api", Query: "UPDATE accounts SET balance = $1"}}},
		Cockroach: &model.CockroachDB{Contention: model.CockroachContention{Hotspots: []model.CockroachContentionHotspot{{
			WaitingStatementFingerprint: "waiter",
		}}}},
	}
	enrichCockroachContention(c)
	if got := c.Cockroach.Contention.Hotspots[0].WaitingQuery; got != "UPDATE accounts SET balance = $1" {
		t.Fatalf("waiting query=%q", got)
	}
	if h := c.Cockroach.Contention.Hotspots[0]; h.WaiterResolution != model.CockroachContentionResolved || len(h.WaitingApplications) != 1 || h.WaitingApplications[0] != "api" {
		t.Fatalf("waiting attribution=%+v", h)
	}
}

func TestAssembleCockroachContentionUnavailable(t *testing.T) {
	d := &model.CockroachDB{}
	assembleCockroachContention(d, conn.Capabilities{}, cockroachSample{})
	if d.Contention.Exactness != model.ExactnessUnavailable {
		t.Fatalf("contention=%+v", d.Contention)
	}
}
