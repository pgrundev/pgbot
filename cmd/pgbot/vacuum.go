package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/pgrundev/pgbot/internal/model"
	"github.com/pgrundev/pgbot/internal/render"
	"github.com/spf13/cobra"
)

// Postgres' default autovacuum trigger: a table is eligible once its dead-tuple
// count passes threshold + scale_factor * live_tuples. Per-table storage params
// can override these, but the cluster defaults cover the overwhelming majority.
const (
	avDefaultThreshold   = 50
	avDefaultScaleFactor = 0.2
)

// newVacuumCmd — `pgbot vacuum`. Autovacuum health per table: dead tuples, when
// autovacuum last ran, and whether Postgres would now expect it to fire. Rising
// dead tuples with expect=yes and no recent run means autovacuum is behind.
func newVacuumCmd() *cobra.Command {
	var f inspectFlags
	cmd := &cobra.Command{
		Use:   "vacuum <connection-string>",
		Short: "Autovacuum health per table — dead tuples, last run, and whether it's due",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVacuum(cmd, args, f)
		},
	}
	fl := cmd.Flags()
	fl.BoolVar(&f.noColor, "no-color", false, "disable ANSI color")
	fl.DurationVar(&f.timeout, "timeout", 30*time.Second, "total wall-clock budget for the run (raise it for slow or remote databases)")
	return cmd
}

func runVacuum(cmd *cobra.Command, args []string, f inspectFlags) error {
	connString := firstNonEmpty(argAt(args, 0), os.Getenv("DATABASE_URL"), os.Getenv("PGBOT_DATABASE_URL"))
	if connString == "" {
		return fmt.Errorf("no connection string (pass one or set $DATABASE_URL)")
	}
	f.ashHz = 0
	f.noStore = true
	f.interval = time.Second

	ctx, cancel := context.WithTimeout(cmd.Context(), f.timeout)
	defer cancel()

	c, host, err := gather(ctx, connString, f)
	if err != nil {
		return err
	}
	if host == "" {
		host = c.Server.Database
	}
	st := render.NewStyler(useColor(f.noColor))

	if c.Tables == nil || len(c.Tables.Top) == 0 {
		fmt.Println(st.Dim("no user tables visible (need SELECT on pg_stat_user_tables)"))
		return nil
	}

	tbls := append([]model.TableStat(nil), c.Tables.Top...)
	sort.SliceStable(tbls, func(i, j int) bool { return tbls[i].DeadTuples > tbls[j].DeadTuples })

	// The trigger is threshold + scale × live_tuples, from the cluster GUCs
	// (collected in the settings tuning set) with per-table reloptions taking
	// precedence. Fall back to Postgres' compiled defaults if settings are absent.
	gThreshold := settingFloat(c, "autovacuum_vacuum_threshold", avDefaultThreshold)
	gScale := settingFloat(c, "autovacuum_vacuum_scale_factor", avDefaultScaleFactor)

	behind := 0
	for _, t := range tbls {
		if expectAutovacuum(t, gThreshold, gScale) {
			behind++
		}
	}

	summary := st.Good("autovacuum keeping up")
	if behind > 0 {
		summary = st.Warn(fmt.Sprintf("%d table(s) past the autovacuum threshold", behind))
	}
	fmt.Printf("%s · %s · %s\n\n", st.Head(host), pgVersionShort(c.Server.VersionNum), summary)

	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "  table\tlive\tdead\tdead%\tlast autovacuum\tdue?")
	for _, t := range tbls {
		due := st.Dim("no")
		if expectAutovacuum(t, gThreshold, gScale) {
			due = st.Warn("yes")
		}
		name := t.Schema + "." + t.Name
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%.1f%%\t%s\t%s\n",
			truncStr(name, 40), humanCount(t.LiveTuples), humanCount(t.DeadTuples),
			t.DeadRatio*100, agoStr(t.LastAutovac), due)
	}
	tw.Flush()
	fmt.Println()
	fmt.Println(st.Dim(fmt.Sprintf("due? = dead tuples past the autovacuum trigger (threshold %.0f + %.0f%% of live rows; per-table overrides applied).",
		gThreshold, gScale*100)))
	fmt.Println(st.Dim("tables ranked by dead tuples, from the 30 largest by size."))
	return nil
}

// expectAutovacuum reports whether a table's dead tuples have passed its
// autovacuum trigger. Per-table reloptions win over the cluster defaults passed
// in; a table with autovacuum_enabled=false will never be autovacuumed, so it is
// never "due" (that is the autovacuum_disabled finding's job, not this column's).
func expectAutovacuum(t model.TableStat, gThreshold, gScale float64) bool {
	if t.AutovacuumDisabled {
		return false
	}
	threshold, scale := gThreshold, gScale
	if t.VacuumThresholdOverride != nil {
		threshold = *t.VacuumThresholdOverride
	}
	if t.VacuumScaleOverride != nil {
		scale = *t.VacuumScaleOverride
	}
	return float64(t.DeadTuples) > threshold+scale*float64(t.LiveTuples)
}

// avVacThreshold / avVacScale are the cluster autovacuum trigger knobs (with the
// compiled-in defaults as fallback), shared by the vacuum command and the MCP
// vacuum_health tool so both grade "due" the same way.
func avVacThreshold(c *model.Context) float64 {
	return settingFloat(c, "autovacuum_vacuum_threshold", avDefaultThreshold)
}
func avVacScale(c *model.Context) float64 {
	return settingFloat(c, "autovacuum_vacuum_scale_factor", avDefaultScaleFactor)
}

// settingFloat reads a numeric parameter from the collected settings, or def.
func settingFloat(c *model.Context, name string, def float64) float64 {
	if c.Settings == nil {
		return def
	}
	if v, ok := c.Settings.Params[name]; ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

// agoStr renders how long ago a timestamp was, or "never" if unset.
func agoStr(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "never"
	}
	d := time.Since(*t)
	switch {
	case d < 0:
		return "just now"
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours())/24)
	}
}
