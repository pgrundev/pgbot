package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/pgrundev/pgbot/internal/render"
	"github.com/spf13/cobra"
)

// newIndexesCmd — `pgbot indexes`. A focused view of zero-scan indexes: what
// they cost and, crucially, the replication caveat before anyone drops one.
// With --correlate it emits the index/code correlation payload (confidence
// levels + what to grep) instead — pgbot never reads the repo; the agent does.
func newIndexesCmd() *cobra.Command {
	var f inspectFlags
	var doCorrelate bool
	cmd := &cobra.Command{
		Use:   "indexes <connection-string>",
		Short: "List indexes with zero scans in the observed window (and what NOT to drop)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runIndexes(cmd, args, f, doCorrelate)
		},
	}
	fl := cmd.Flags()
	fl.BoolVar(&f.noColor, "no-color", false, "disable ANSI color")
	fl.BoolVar(&f.noStore, "no-store", true, "skip the local baseline store (default for a quick listing)")
	fl.StringVar(&f.storePath, "store", "", "baseline DB path (default: XDG state dir)")
	fl.DurationVar(&f.timeout, "timeout", 30*time.Second, "total wall-clock budget for the run (raise it for slow or remote databases)")
	fl.BoolVar(&doCorrelate, "correlate", false, "emit the index/code correlation payload: confidence levels + identifiers to search for in code (pgbot never reads the repo)")
	fl.BoolVar(&f.json, "json", false, "JSON output (implies --correlate; the machine-readable correlation payload)")
	return cmd
}

func runIndexes(cmd *cobra.Command, args []string, f inspectFlags, doCorrelate bool) error {
	connString := firstNonEmpty(argAt(args, 0), os.Getenv("DATABASE_URL"), os.Getenv("PGBOT_DATABASE_URL"))
	if connString == "" {
		return fmt.Errorf("no connection string (pass one or set $DATABASE_URL)")
	}
	f.interval = time.Second
	f.ashHz = 0 // no wait profile needed for an index listing

	ctx, cancel := context.WithTimeout(cmd.Context(), f.timeout)
	defer cancel()

	c, host, err := gather(ctx, connString, f)
	if err != nil {
		return err
	}

	st := render.NewStyler(useColor(f.noColor))

	// --correlate (or --json, which implies it) switches to the correlation
	// payload. The default listing below is unchanged.
	if doCorrelate || f.json {
		rep := buildCorrelation(c, f.storePath)
		if f.json {
			b, err := json.MarshalIndent(rep, "", "  ")
			if err != nil {
				return err
			}
			fmt.Println(string(b))
			return nil
		}
		renderCorrelation(os.Stdout, st, rep)
		return nil
	}
	if c.Server.Engine == "cockroachdb" {
		return render.CockroachScreen(os.Stdout, c, "indexes", render.Options{
			Color: useColor(f.noColor), Host: host, Width: terminalWidth(), Full: true,
		})
	}

	if c.Indexes == nil || len(c.Indexes.Unused) == 0 {
		fmt.Println(st.Good("✓ no zero-scan indexes in this window"))
		return nil
	}

	var total int64
	for _, ix := range c.Indexes.Unused {
		total += ix.Bytes
	}
	fmt.Printf("%s\n\n", st.Head(fmt.Sprintf("%d indexes with zero scans in this window — %s total",
		len(c.Indexes.Unused), render.HumanBytes(total))))

	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "  size\tindex\ttable")
	shown := len(c.Indexes.Unused)
	if shown > 12 {
		shown = 12
	}
	for _, ix := range c.Indexes.Unused[:shown] {
		tag := ""
		switch {
		case ix.Partial:
			tag = " [partial]"
		case ix.Expression:
			tag = " [expression]"
		}
		fmt.Fprintf(tw, "  %s\t%s%s\t%s\n", render.HumanBytes(ix.Bytes), ix.Name, tag, ix.Schema+"."+ix.Table)
	}
	tw.Flush()
	if more := len(c.Indexes.Unused) - shown; more > 0 {
		fmt.Printf("  %s\n", st.Dim(fmt.Sprintf("…%d more", more)))
	}
	fmt.Println()

	// The load-bearing warnings, front and center.
	if c.Window.ColdWindow() {
		fmt.Println(st.AI("⚠ cold statistics window — these scan counts just started from zero."))
		fmt.Println(st.Dim("  Wait until the window exceeds 15m before trusting 'unused'."))
	}
	if c.Replication != nil && (len(c.Replication.Replicas) > 0 || c.Replication.IsReplica) {
		fmt.Println(st.AI("⚠ replication is active — replicas may use these."))
		fmt.Println(st.Dim("  Check replica index stats before dropping anything."))
	}
	return nil
}
