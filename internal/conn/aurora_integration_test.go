package conn

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestIntegration_auroraInstances is the opt-in end-to-end check for
// --all-instances discovery against a real Aurora cluster: list the members,
// derive each instance endpoint from the DSN's host, connect to each, and prove
// the derived endpoint reached the member it names. Run it as
//
//	PGBOT_AURORA_TEST_DSN="postgres://user:pw@cluster.cluster-xyz.region.rds.amazonaws.com/db?sslmode=require" \
//	  go test ./internal/conn/ -run TestIntegration_auroraInstances -v
func TestIntegration_auroraInstances(t *testing.T) {
	dsn := os.Getenv("PGBOT_AURORA_TEST_DSN")
	if dsn == "" {
		t.Skip("set PGBOT_AURORA_TEST_DSN (a native Aurora cluster endpoint) to run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	entry, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer entry.Close()
	if entry.Caps.Provider != ProviderAurora {
		t.Fatalf("provider detected as %q, want aurora", entry.Caps.Provider)
	}
	instances, err := AuroraInstances(ctx, entry)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := CanonicalRDSHost(cfg.ConnConfig.Host)
	if err != nil {
		t.Fatalf("endpoint: %v", err)
	}
	t.Logf("entry endpoint %s → %s; %d instance(s)", cfg.ConnConfig.Host, endpoint, len(instances))

	for _, inst := range instances {
		host, err := AuroraInstanceHost(endpoint, inst.ID)
		if err != nil {
			t.Fatalf("%s: derive endpoint: %v", inst.ID, err)
		}
		member, err := ConnectDBAt(ctx, dsn, "", host)
		if err != nil {
			t.Errorf("%s (%s): connect to %s: %v", inst.ID, inst.Role(), host, err)
			continue
		}
		got, err := AuroraInstanceIdentifier(ctx, member)
		member.Close()
		if err != nil {
			t.Errorf("%s: %v", inst.ID, err)
			continue
		}
		if got != inst.ID {
			t.Errorf("%s reached instance %q — endpoint derivation is wrong for this cluster", host, got)
			continue
		}
		t.Logf("✓ %-8s %s at %s (in_recovery=%v)", inst.Role(), inst.ID, host, member.Caps.InRecovery)
	}
}
