package conn

import (
	"strings"
	"testing"
)

func TestAuroraInstanceHost(t *testing.T) {
	cases := []struct {
		endpoint, instance, want, wantErr string
	}{
		{"prod.cluster-abc123.us-east-1.rds.amazonaws.com", "prod-instance-1", "prod-instance-1.abc123.us-east-1.rds.amazonaws.com", ""},
		{"prod.cluster-ro-abc123.us-east-1.rds.amazonaws.com", "prod-instance-2", "prod-instance-2.abc123.us-east-1.rds.amazonaws.com", ""},
		{"analytics.cluster-custom-abc123.us-east-1.rds.amazonaws.com", "prod-instance-3", "prod-instance-3.abc123.us-east-1.rds.amazonaws.com", ""},
		// An instance endpoint already carries the bare cluster id.
		{"prod-instance-1.abc123.us-east-1.rds.amazonaws.com", "prod-instance-2", "prod-instance-2.abc123.us-east-1.rds.amazonaws.com", ""},
		// GovCloud and China partitions keep the shape.
		{"prod.cluster-abc123.us-gov-west-1.rds.amazonaws.com", "i1", "i1.abc123.us-gov-west-1.rds.amazonaws.com", ""},
		{"prod.cluster-abc123.cn-north-1.rds.amazonaws.com.cn", "i1", "i1.abc123.cn-north-1.rds.amazonaws.com.cn", ""},
		// Case and a trailing dot are DNS noise.
		{"Prod.Cluster-ABC123.us-east-1.rds.amazonaws.com.", "Prod-Instance-1", "prod-instance-1.abc123.us-east-1.rds.amazonaws.com", ""},
		// Refused: proxies hide members; non-RDS names and IPs have nothing to derive from.
		{"prod.proxy-abc123.us-east-1.rds.amazonaws.com", "i1", "", "RDS Proxy"},
		{"db.internal.example.com", "i1", "", "not an RDS endpoint"},
		{"10.0.0.5", "i1", "", "IP address"},
		{"prod.cluster-abc123.us-east-1.rds.amazonaws.com", "", "", "empty Aurora instance identifier"},
	}
	for _, c := range cases {
		got, err := AuroraInstanceHost(c.endpoint, c.instance)
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("AuroraInstanceHost(%q, %q) = %q, %v; want error containing %q", c.endpoint, c.instance, got, err, c.wantErr)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("AuroraInstanceHost(%q, %q) = %q, %v; want %q", c.endpoint, c.instance, got, err, c.want)
		}
	}
}

func TestAuroraInstancesFromStatus(t *testing.T) {
	got, err := auroraInstancesFromStatus([]auroraStatusRow{
		{"prod-reader-b", "1f2e3d4c"},
		{"prod-writer", "MASTER_SESSION_ID"},
		{"prod-reader-a", "9a8b7c6d"},
		{"  ", ""}, // a blank row is ignored, not an instance
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []AuroraInstance{{ID: "prod-writer", Writer: true}, {ID: "prod-reader-a"}, {ID: "prod-reader-b"}}
	if len(got) != len(want) {
		t.Fatalf("got %d instances, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].ID != want[i].ID || got[i].Writer != want[i].Writer {
			t.Errorf("instance %d = %+v, want %+v (writer first, then readers by id)", i, got[i], want[i])
		}
	}
	if got[0].Role() != "writer" || got[1].Role() != "reader" {
		t.Errorf("roles = %s/%s", got[0].Role(), got[1].Role())
	}

	// Anything but exactly one writer is refused rather than mislabeled.
	if _, err := auroraInstancesFromStatus([]auroraStatusRow{{"a", "x"}, {"b", "y"}}); err == nil {
		t.Error("no writer accepted")
	}
	if _, err := auroraInstancesFromStatus([]auroraStatusRow{{"a", "MASTER_SESSION_ID"}, {"b", "MASTER_SESSION_ID"}}); err == nil {
		t.Error("two writers accepted")
	}
	if _, err := auroraInstancesFromStatus(nil); err == nil {
		t.Error("empty status accepted")
	}
}
