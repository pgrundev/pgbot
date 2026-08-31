package main

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/pgrundev/pgbot/internal/render"
	"github.com/spf13/cobra"
)

// newTablesCmd — `pgbot tables`. The largest tables by total size (heap + indexes
// + TOAST), each with its row count, dead-tuple ratio, and sequential-vs-index
// scan counts — so it doubles as a "big table getting seq-scanned, probably
// missing an index" view, not just storage accounting. Read-only.
func newTablesCmd() *cobra.Command {
	var f inspectFlags
	cmd := &cobra.Command{
		Use:   "tables <connection-string>",
		Short: "Largest tables by total size, with row counts and scan patterns",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTables(cmd, args, f)
		},
	}
	fl := cmd.Flags()
	fl.BoolVar(&f.noColor, "no-color", false, "disable ANSI color")
	fl.DurationVar(&f.timeout, "timeout", 30*time.Second, "total wall-clock budget for the run (raise it for slow or remote databases)")
	fl.StringVar(&f.crdbAdminURL, "crdb-admin-url", "", "CockroachDB DB Console/Admin API origin (or PGBOT_CRDB_ADMIN_URL)")
	fl.StringVar(&f.crdbPromURL, "crdb-prometheus-url", "", "CockroachDB Prometheus origin or /_status/load URL (defaults to admin URL)")
	return cmd
}

func runTables(cmd *cobra.Command, args []string, f inspectFlags) error {
	connString := firstNonEmpty(argAt(args, 0), os.Getenv("DATABASE_URL"), os.Getenv("PGBOT_DATABASE_URL"))
	if connString == "" {
		return fmt.Errorf("no connection string (pass one or set $DATABASE_URL)")
	}
	f.ashHz = 0
	f.noStore = true
	f.interval = time.Second
	f.crdbHTTP = true

	ctx, cancel := context.WithTimeout(cmd.Context(), f.timeout)
	defer cancel()

	c, host, err := gather(ctx, connString, f)
	if err != nil {
		return err
	}
	if host == "" {
		host = c.Server.Database
	}
	if c.Server.Engine == "cockroachdb" {
		return render.CockroachScreen(os.Stdout, c, "tables", render.Options{
			Color: useColor(f.noColor), Host: host, Width: terminalWidth(), Full: true,
		})
	}
	st := render.NewStyler(useColor(f.noColor))

	if c.Tables == nil || len(c.Tables.Top) == 0 {
		fmt.Println(st.Dim("no user tables visible (need SELECT on pg_stat_user_tables)"))
		return nil
	}

	dbsize := ""
	if c.Tables.DBSizeBytes > 0 {
		dbsize = " · " + render.HumanBytes(c.Tables.DBSizeBytes) + " database"
	}
	fmt.Printf("%s · %s · %s%s\n\n", st.Head(host), pgVersionShort(c.Server.VersionNum),
		st.Dim(fmt.Sprintf("top %d tables by size", len(c.Tables.Top))), st.Dim(dbsize))

	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "  size\trows\tdead%\tseq scans\tidx scans\ttable")
	for _, t := range c.Tables.Top {
		fmt.Fprintf(tw, "  %s\t%s\t%.1f%%\t%s\t%s\t%s\n",
			render.HumanBytes(t.TotalBytes), humanCount(t.LiveTuples), t.DeadRatio*100,
			humanCount(t.SeqScans), humanCount(t.IndexScans), t.Schema+"."+t.Name)
	}
	tw.Flush()
	fmt.Println()
	fmt.Println(st.Dim("size = heap + indexes + TOAST. seq/idx = cumulative scan counts — a large table"))
	fmt.Println(st.Dim("with heavy seq scans and few index scans is a likely missing-index candidate."))
	return nil
}
