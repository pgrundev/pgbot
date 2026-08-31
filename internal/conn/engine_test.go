package conn

import "testing"

func TestDetectEngine(t *testing.T) {
	for _, tc := range []struct {
		version string
		want    Engine
	}{
		{"PostgreSQL 17.4 on x86_64-pc-linux-gnu", EnginePostgreSQL},
		{"CockroachDB CCL v26.4.0 (darwin arm64)", EngineCockroachDB},
		{"cockroachdb v24.3.0", EngineCockroachDB},
	} {
		if got := detectEngine(tc.version); got != tc.want {
			t.Errorf("detectEngine(%q) = %q, want %q", tc.version, got, tc.want)
		}
	}
}

func TestCockroachDoesNotInheritPostgresVersionCapabilities(t *testing.T) {
	c := Capabilities{Engine: EngineCockroachDB, VersionNum: 180000}
	if c.HasStatWAL() || c.HasStatIO() || c.HasStatCheckpointer() || c.HasStatsFetchConsistency() {
		t.Error("CockroachDB's PostgreSQL compatibility version must not enable PostgreSQL collectors")
	}
	if (Capabilities{VersionNum: 180000}).EngineName() != EnginePostgreSQL {
		t.Error("zero-value engine should remain PostgreSQL for backward compatibility")
	}
}
