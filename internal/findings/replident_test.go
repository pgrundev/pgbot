package findings

import (
	"strings"
	"testing"

	"github.com/pgrundev/pgbot/internal/model"
)

func TestReplicaIdentityMissing(t *testing.T) {
	ctx := &model.Context{ReplicaIdentity: &model.ReplicaIdentity{Unidentifiable: []model.PublishedTable{
		{Schema: "public", Name: "events", Identity: "d", Publications: "app_pub"},
		{Schema: "public", Name: "audit", Identity: "n", Publications: "app_pub, other_pub"},
	}}}
	f := has(Compute(ctx), "replica_identity_missing")
	if f == nil || f.Severity != model.SeverityCritical {
		t.Fatalf("expected critical replica_identity_missing, got %+v", f)
	}
	if got := []string{"public.events", "public.audit"}; len(f.Objects) != 2 || f.Objects[0] != got[0] || f.Objects[1] != got[1] {
		t.Errorf("objects must be relations, aligned with evidence: %v", f.Objects)
	}
	if !strings.Contains(f.Evidence[0], "no primary key") || !strings.Contains(f.Evidence[0], "app_pub") {
		t.Errorf("evidence must name the reason and the publication: %q", f.Evidence[0])
	}
	if !strings.Contains(f.Evidence[1], "nothing") {
		t.Errorf("an explicit REPLICA IDENTITY NOTHING must say so: %q", f.Evidence[1])
	}
	if len(f.Caveats) == 0 || !strings.Contains(f.Caveats[0], "FULL") {
		t.Errorf("must carry the REPLICA IDENTITY FULL cost caveat: %v", f.Caveats)
	}

	idx := &model.Context{ReplicaIdentity: &model.ReplicaIdentity{Unidentifiable: []model.PublishedTable{
		{Schema: "app", Name: "t", Identity: "i", Publications: "p"},
	}}}
	if f := has(Compute(idx), "replica_identity_missing"); f == nil || !strings.Contains(f.Evidence[0], "invalid") {
		t.Errorf("a dangling REPLICA IDENTITY USING INDEX must be reported: %+v", f)
	}

	for _, healthy := range []*model.Context{
		{ReplicaIdentity: &model.ReplicaIdentity{}},
		{ReplicaIdentity: &model.ReplicaIdentity{Section: model.Section{Exactness: model.ExactnessUnavailable}}},
		{},
	} {
		if has(Compute(healthy), "replica_identity_missing") != nil {
			t.Error("no unidentifiable published tables must not fire")
		}
	}
}
