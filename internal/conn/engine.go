package conn

import "strings"

// Engine identifies the SQL engine behind the PostgreSQL wire protocol.
// CockroachDB intentionally reports a PostgreSQL-compatible server version, so
// feature gates must use this field rather than VersionNum alone.
type Engine string

const (
	EnginePostgreSQL  Engine = "postgresql"
	EngineCockroachDB Engine = "cockroachdb"
)

func detectEngine(versionText string) Engine {
	if strings.Contains(strings.ToLower(versionText), "cockroachdb") {
		return EngineCockroachDB
	}
	return EnginePostgreSQL
}

// EngineName keeps zero-value Capabilities backward-compatible with callers
// and tests that predate explicit engine detection.
func (c Capabilities) EngineName() Engine {
	if c.Engine == "" {
		return EnginePostgreSQL
	}
	return c.Engine
}

func (c Capabilities) IsCockroachDB() bool { return c.EngineName() == EngineCockroachDB }
