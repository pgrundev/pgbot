package findings

import (
	"strings"
	"testing"

	"github.com/pgrundev/pgbot/internal/model"
)

func TestReplicaLagTime_warningThresholdCannotHideCritical(t *testing.T) {
	cases := []struct {
		name     string
		warn     float64
		lag      float64
		severity string
	}{
		{"below default warning", 60, 59, ""},
		{"at default warning", 60, 60, model.SeverityWarn},
		{"below critical", 60, 299, model.SeverityWarn},
		{"at critical", 60, 300, model.SeverityCritical},
		{"lower warning", 30, 30, model.SeverityWarn},
		{"below raised warning", 180, 179, ""},
		{"at raised warning", 180, 180, model.SeverityWarn},
		{"warning equals critical", 300, 300, model.SeverityCritical},
		{"high warning suppresses only subcritical lag", 600, 299, ""},
		{"high warning cannot hide critical boundary", 600, 300, model.SeverityCritical},
		{"high warning cannot hide critical lag", 600, 450, model.SeverityCritical},
		{"lag reaches high warning", 600, 600, model.SeverityCritical},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			flow := 1000.0
			c := &model.Context{
				WAL: &model.WAL{BytesPerSec: &flow},
				Replication: &model.Replication{Replicas: []model.ReplicaRow{
					{AppName: "standby", ReplayLagSec: &tc.lag},
				}},
			}
			tun := DefaultTunables()
			tun.ReplicaLagWarnSec = tc.warn
			f := has(ComputeWithTunables(c, tun), "replica_lag_time")
			if tc.severity == "" {
				if f != nil {
					t.Fatalf("unexpected lag finding: %+v", f)
				}
				return
			}
			if f == nil || f.Severity != tc.severity {
				t.Fatalf("warning threshold %g, lag %g: got %+v, want %s", tc.warn, tc.lag, f, tc.severity)
			}
		})
	}
}

func TestReplicaLagTime_highWarningPreservesMissingAndIdleGuards(t *testing.T) {
	lag, idle, flowing := 450.0, 0.0, 1000.0
	for _, wal := range []*model.WAL{nil, {}, {BytesPerSec: &idle}} {
		c := &model.Context{
			WAL: wal,
			Replication: &model.Replication{Replicas: []model.ReplicaRow{
				{AppName: "standby", ReplayLagSec: &lag},
			}},
		}
		tun := DefaultTunables()
		tun.ReplicaLagWarnSec = 600
		if f := has(ComputeWithTunables(c, tun), "replica_lag_time"); f != nil {
			t.Errorf("unmeasured or idle WAL must still suppress lag: %+v", f)
		}
	}
	c := &model.Context{
		WAL: &model.WAL{BytesPerSec: &flowing},
		Replication: &model.Replication{Replicas: []model.ReplicaRow{
			{AppName: "no-lag-measurement"},
		}},
	}
	tun := DefaultTunables()
	tun.ReplicaLagWarnSec = 600
	if f := has(ComputeWithTunables(c, tun), "replica_lag_time"); f != nil {
		t.Errorf("unknown replay lag must not produce a finding: %+v", f)
	}
}

func TestReplicaLagTime_highWarningReportsWorstCriticalReplica(t *testing.T) {
	flow, low, high := 1000.0, 300.0, 450.0
	c := &model.Context{
		WAL: &model.WAL{BytesPerSec: &flow},
		Replication: &model.Replication{Replicas: []model.ReplicaRow{
			{AppName: "unknown"},
			{AppName: "first", ReplayLagSec: &low},
			{AppName: "worst", ReplayLagSec: &high},
		}},
	}
	tun := DefaultTunables()
	tun.ReplicaLagWarnSec = 600
	f := has(ComputeWithTunables(c, tun), "replica_lag_time")
	if f == nil || f.Severity != model.SeverityCritical {
		t.Fatalf("critical lag was hidden by the warning threshold: %+v", f)
	}
	if !strings.Contains(f.Title, "450s") || len(f.Evidence) != 2 {
		t.Errorf("must report worst measured lag and both critical replicas: %+v", f)
	}
}
