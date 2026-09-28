package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/pgrundev/pgbot/internal/model"
	"github.com/pgrundev/pgbot/internal/render"
	"github.com/spf13/cobra"
)

var gatherQueryContext = gather

const queryStatsReadFailed = "pg_stat_statements read failed"

// newQueriesCmd — `pgbot queries`. The top statements from pg_stat_statements,
// ranked by total execution time (the query quietly eating the database) or by
// call count with --by-calls (a cheap query run a million times). Read-only.
func newQueriesCmd() *cobra.Command {
	var f inspectFlags
	var byCalls bool
	cmd := &cobra.Command{
		Use:   "queries <connection-string>",
		Short: "Top queries by total execution time (or --by-calls), from pg_stat_statements",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runQueries(cmd, args, f, byCalls)
		},
	}
	fl := cmd.Flags()
	fl.BoolVar(&f.noColor, "no-color", false, "disable ANSI color")
	fl.BoolVar(&byCalls, "by-calls", false, "rank by call count instead of total execution time")
	fl.DurationVar(&f.timeout, "timeout", 30*time.Second, "total wall-clock budget for the run (raise it for slow or remote databases)")
	return cmd
}

func runQueries(cmd *cobra.Command, args []string, f inspectFlags, byCalls bool) error {
	connString := firstNonEmpty(argAt(args, 0), os.Getenv("DATABASE_URL"), os.Getenv("PGBOT_DATABASE_URL"), pgServiceFallback())
	if connString == "" {
		return fmt.Errorf("no connection string (pass one or set $DATABASE_URL)")
	}
	f.ashHz = 0
	f.noStore = true
	f.interval = time.Second

	ctx, cancel := context.WithTimeout(cmd.Context(), f.timeout)
	defer cancel()

	c, host, err := gatherQueryContext(ctx, connString, f)
	if err != nil {
		return err
	}
	if host == "" {
		host = c.Server.Database
	}
	st := render.NewStyler(useColor(f.noColor))
	out := cmd.OutOrStdout()

	if c.Queries == nil || !c.Queries.Enabled {
		reason := "pg_stat_statements not enabled"
		if c.Queries != nil && c.Queries.Reason != "" {
			reason = c.Queries.Reason
		}
		fmt.Fprintln(out, st.Warn("pg_stat_statements is required for query stats."))
		fmt.Fprintln(out, st.Dim("  "+reason))
		return nil
	}
	if c.Queries.Exactness == model.ExactnessUnavailable {
		fmt.Fprintln(out, st.Warn("Query stats are unavailable."))
		fmt.Fprintln(out, st.Dim("  "+queryStatsReadFailed))
		return nil
	}

	top := append([]model.QueryStat(nil), c.Queries.Top...)
	label := "by total execution time"
	if byCalls {
		sort.SliceStable(top, func(i, j int) bool { return top[i].Calls > top[j].Calls })
		label = "by call count"
	}

	fmt.Fprintf(out, "%s · %s · top %d queries %s\n\n", st.Head(host), pgVersionShort(c.Server.VersionNum), len(top), st.Dim(label))
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "  total\tshare\tcalls\tmean\tquery")
	for _, q := range top {
		share := "—"
		if c.Queries.TotalExecMS > 0 {
			share = fmt.Sprintf("%.1f%%", q.TotalMS/c.Queries.TotalExecMS*100)
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%.2f ms\t%s\n",
			durFromMs(q.TotalMS), share, humanCount(q.Calls), q.MeanMS, truncStr(q.Query, 60))
	}
	tw.Flush()
	fmt.Fprintln(out)
	fmt.Fprintln(out, st.Dim("share = % of total execution time across all statements. `pgbot inspect --json` for the full set."))
	return nil
}

// durFromMs renders a millisecond total as a coarse human duration.
func durFromMs(ms float64) string {
	d := time.Duration(ms * float64(time.Millisecond))
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	case d >= time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	case d >= time.Second:
		return fmt.Sprintf("%.1fs", d.Seconds())
	default:
		return fmt.Sprintf("%.0fms", ms)
	}
}

func humanCount(n int64) string {
	switch {
	case n >= 1e9:
		return fmt.Sprintf("%.1fB", float64(n)/1e9)
	case n >= 1e6:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1e3:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func truncStr(s string, n int) string {
	// Collapse internal whitespace (multi-line SQL) and truncate by RUNE, so a
	// multibyte character is never split — a split rune misaligns tabwriter
	// columns and prints a replacement glyph (PR#1).
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n || n < 1 {
		return s
	}
	return string(r[:n-1]) + "…"
}
