package conn

import "strings"

// Provider identifies the managed Postgres platform, so pgbot can give a
// provider-specific fix when something needs enabling (notably
// pg_stat_statements, which lands on the degraded path for a large fraction of
// first runs on RDS/Aurora). Detected from the host, version() text, and the
// presence of provider-specific settings — the settings markers make it work
// even when the host is an IP or behind a proxy.
type Provider string

const (
	ProviderRDS      Provider = "rds"
	ProviderAurora   Provider = "aurora"
	ProviderCloudSQL Provider = "cloudsql"
	ProviderAzure    Provider = "azure"
	ProviderSupabase Provider = "supabase"
	ProviderNeon     Provider = "neon"
	ProviderUnknown  Provider = "unknown"
)

// providerMarkers are the best-effort signals gathered during the probe.
type providerMarkers struct {
	Host        string
	VersionText string
	HasRDS      bool // a pg_settings row named rds.*
	HasCloudSQL bool // cloudsql.*
	HasAzure    bool // azure.*
	IsAurora    bool // aurora_version() exists in pg_proc
}

func detectProvider(m providerMarkers) Provider {
	h := strings.ToLower(m.Host)
	vt := strings.ToLower(m.VersionText)
	switch {
	case strings.Contains(h, "supabase"):
		return ProviderSupabase
	case strings.Contains(h, "neon.tech"):
		return ProviderNeon
	case strings.Contains(h, "postgres.database.azure.com") || m.HasAzure:
		return ProviderAzure
	case m.IsAurora || strings.Contains(vt, "aurora"):
		return ProviderAurora
	case strings.Contains(h, "rds.amazonaws.com") || m.HasRDS:
		return ProviderRDS
	case m.HasCloudSQL:
		return ProviderCloudSQL
	}
	return ProviderUnknown
}

// PgssInstructions returns the copy-pasteable steps to enable
// pg_stat_statements on this provider.
func (p Provider) PgssInstructions() string {
	switch p {
	case ProviderRDS, ProviderAurora:
		return "add pg_stat_statements to shared_preload_libraries in the DB parameter group and reboot the instance, then: CREATE EXTENSION pg_stat_statements;"
	case ProviderCloudSQL:
		return "set the cloudsql.enable_pg_stat_statements flag on the instance, then: CREATE EXTENSION pg_stat_statements;"
	case ProviderAzure:
		return "add pg_stat_statements to both the azure.extensions and shared_preload_libraries server parameters (a restart applies the latter), then: CREATE EXTENSION pg_stat_statements;"
	case ProviderSupabase, ProviderNeon:
		return "it's preloaded here — just run: CREATE EXTENSION pg_stat_statements;"
	default:
		return "add pg_stat_statements to shared_preload_libraries in postgresql.conf and restart, then: CREATE EXTENSION pg_stat_statements;"
	}
}
