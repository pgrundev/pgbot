package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pgrundev/pggo"
)

// The fallback hands the driver a bare "service=<name>" and relies on it to read the
// connection service file. Pin that end to end: a service file named through
// PGSERVICEFILE must supply host, port, user, and database, and a service name
// missing from the file must be an error rather than a silent localhost.
func TestPgServiceFallback_resolvesThroughServiceFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "pg_service.conf")
	if err := os.WriteFile(file, []byte("[prod-ro]\nhost=db.internal\nport=6432\nuser=pgbot_ro\ndbname=appdb\nsslmode=require\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PGSERVICEFILE", file)
	t.Setenv("PGSERVICE", "prod-ro")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("PGBOT_DATABASE_URL", "")
	// The service file must be the only source of these.
	for _, v := range []string{"PGHOST", "PGPORT", "PGUSER", "PGDATABASE", "PGSSLMODE"} {
		t.Setenv(v, "")
	}

	dsn := firstNonEmpty("", os.Getenv("DATABASE_URL"), os.Getenv("PGBOT_DATABASE_URL"), pgServiceFallback())
	cfg, err := pggo.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("pggo.ParseConfig(%q): %v", dsn, err)
	}
	if cfg.Host != "db.internal" || cfg.Port != 6432 || cfg.User != "pgbot_ro" || cfg.Database != "appdb" {
		t.Fatalf("service file not applied: host=%q port=%d user=%q db=%q", cfg.Host, cfg.Port, cfg.User, cfg.Database)
	}
	if cfg.SSLMode != "require" {
		t.Fatal("sslmode=require from the service file was not applied")
	}

	t.Setenv("PGSERVICE", "does-not-exist")
	if _, err := pggo.ParseConfig(pgServiceFallback()); err == nil {
		t.Fatal("an unknown service name should fail to parse, not fall through to defaults")
	}
}
