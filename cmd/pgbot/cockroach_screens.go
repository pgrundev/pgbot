package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/pgrundev/pgbot/internal/render"
	"github.com/spf13/cobra"
)

type cockroachScreenSpec struct {
	name  string
	short string
}

var cockroachScreenSpecs = []cockroachScreenSpec{
	{name: "health", short: "CockroachDB node, range, capacity, and resource health"},
	{name: "distribution", short: "CockroachDB replica, lease, capacity, and hotspot balance"},
	{name: "storage", short: "CockroachDB storage, MVCC, recovery, and Raft health"},
	{name: "jobs", short: "CockroachDB background jobs and schema-change health"},
	{name: "activity", short: "CockroachDB sessions and currently running queries"},
	{name: "contention", short: "CockroachDB lock contention and serialization hotspots"},
}

func newCockroachScreenCommands() []*cobra.Command {
	commands := make([]*cobra.Command, 0, len(cockroachScreenSpecs))
	for _, spec := range cockroachScreenSpecs {
		commands = append(commands, newCockroachScreenCmd(spec))
	}
	return commands
}

func newCockroachScreenCmd(spec cockroachScreenSpec) *cobra.Command {
	var f inspectFlags
	cmd := &cobra.Command{
		Use:   spec.name + " <connection-string>",
		Short: spec.short,
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCockroachScreen(cmd, args, f, spec.name)
		},
	}
	fl := cmd.Flags()
	fl.BoolVar(&f.noColor, "no-color", false, "disable ANSI color")
	fl.DurationVar(&f.interval, "interval", time.Second, "gap between the two counter samples (min 500ms)")
	fl.DurationVar(&f.timeout, "timeout", 30*time.Second, "total wall-clock budget for the run (raise it for slow or remote databases)")
	fl.StringVar(&f.crdbAdminURL, "crdb-admin-url", "", "CockroachDB DB Console/Admin API origin (or PGBOT_CRDB_ADMIN_URL)")
	fl.StringVar(&f.crdbPromURL, "crdb-prometheus-url", "", "CockroachDB Prometheus origin or /_status/load URL (defaults to admin URL)")
	return cmd
}

func runCockroachScreen(cmd *cobra.Command, args []string, f inspectFlags, name string) error {
	connString := firstNonEmpty(argAt(args, 0), os.Getenv("DATABASE_URL"), os.Getenv("PGBOT_DATABASE_URL"))
	if connString == "" {
		return fmt.Errorf("no connection string (pass one or set $DATABASE_URL)")
	}
	f.ashHz = 0
	f.noStore = true
	f.crdbHTTP = true

	ctx, cancel := context.WithTimeout(cmd.Context(), f.timeout)
	defer cancel()
	c, host, err := gather(ctx, connString, f)
	if err != nil {
		return err
	}
	if c.Server.Engine != "cockroachdb" {
		return fmt.Errorf("%s is a CockroachDB-only command; use inspect for PostgreSQL", name)
	}
	return render.CockroachScreen(os.Stdout, c, name, render.Options{
		Color: useColor(f.noColor), Host: host, Width: terminalWidth(), Full: true,
	})
}
