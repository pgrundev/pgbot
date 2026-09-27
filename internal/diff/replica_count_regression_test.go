package diff

import (
	"testing"

	"github.com/pgrundev/pgbot/internal/model"
)

func TestReplicaCountLossControls(t *testing.T) {
	for _, tc := range []struct {
		name          string
		before, after []model.ReplicaRow
		want          map[string][2]float64
	}{
		{"unchanged duplicate group", []model.ReplicaRow{{AppName: "a"}, {AppName: "a"}}, []model.ReplicaRow{{AppName: "a"}, {AppName: "a"}}, nil},
		{"growing duplicate group", []model.ReplicaRow{{AppName: "a"}}, []model.ReplicaRow{{AppName: "a"}, {AppName: "a"}}, nil},
		{"duplicate group disappears", []model.ReplicaRow{{AppName: "a"}, {AppName: "a"}}, nil, map[string][2]float64{"a": {2, 0}}},
		{"distinct groups", []model.ReplicaRow{{AppName: "a"}, {AppName: "a"}, {AppName: "b"}}, []model.ReplicaRow{{AppName: "a"}}, map[string][2]float64{"a": {2, 1}, "b": {1, 0}}},
		{"address fallback", []model.ReplicaRow{{ClientAddr: "192.0.2.1"}, {ClientAddr: "192.0.2.1"}}, []model.ReplicaRow{{ClientAddr: "192.0.2.1"}}, map[string][2]float64{"192.0.2.1": {2, 1}}},
		{"unknown identity", []model.ReplicaRow{{}, {}}, nil, nil},
		{"named replica address changes", []model.ReplicaRow{{AppName: "a", ClientAddr: "192.0.2.1"}}, []model.ReplicaRow{{AppName: "a", ClientAddr: "192.0.2.2"}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := &model.Context{Replication: &model.Replication{Replicas: tc.before}}
			now := &model.Context{Replication: &model.Replication{Replicas: tc.after}}
			d := Compute(now, &Baseline{Context: base}, nil)
			got := map[string][2]float64{}
			for _, c := range d.Changes {
				if c.ID != "replication.standby_gone" {
					continue
				}
				if _, exists := got[c.Subject]; exists {
					t.Fatalf("duplicate delta for %s", c.Subject)
				}
				got[c.Subject] = [2]float64{c.Before, c.After}
			}
			if len(got) != len(tc.want) {
				t.Fatalf("deltas = %v, want %v", got, tc.want)
			}
			for key, pair := range tc.want {
				if got[key] != pair {
					t.Errorf("%s: got %v, want %v", key, got[key], pair)
				}
			}
		})
	}
}

func TestReplicaCountMissingSectionsRemainUnavailable(t *testing.T) {
	withReplica := &model.Context{Replication: &model.Replication{Replicas: []model.ReplicaRow{{AppName: "a"}}}}
	for _, pair := range [][2]*model.Context{{{}, withReplica}, {withReplica, {}}} {
		d := Compute(pair[0], &Baseline{Context: pair[1]}, nil)
		if c := change(d, "replication.standby_gone"); c != nil {
			t.Fatalf("a missing section is not evidence of a lost replica: %+v", c)
		}
	}
}
