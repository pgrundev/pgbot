package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/pgrundev/pgbot/internal/findings"
	"github.com/pgrundev/pgbot/internal/model"
	"github.com/pgrundev/pgbot/internal/render"
	"github.com/spf13/cobra"
)

// newTuneCmd — `pgbot tune`. Deterministic configuration-tuning recommendations
// derived from the observed workload (temp-file spill → work_mem, forced
// checkpoints → max_wal_size, over-provisioned connections). Read-only; it
// recommends, it never changes anything.
func newTuneCmd() *cobra.Command {
	var f inspectFlags
	cmd := &cobra.Command{
		Use:   "tune <connection-string>",
		Short: "Config-tuning recommendations from the observed workload (read-only)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTune(cmd, args, f)
		},
	}
	fl := cmd.Flags()
	fl.BoolVar(&f.noColor, "no-color", false, "disable ANSI color")
	fl.DurationVar(&f.interval, "interval", time.Second, "gap between the two counter samples (min 500ms)")
	fl.DurationVar(&f.timeout, "timeout", 30*time.Second, "total wall-clock budget for the run (raise it for slow or remote databases)")
	return cmd
}

func runTune(cmd *cobra.Command, args []string, f inspectFlags) error {
	connString := firstNonEmpty(argAt(args, 0), os.Getenv("DATABASE_URL"), os.Getenv("PGBOT_DATABASE_URL"))
	if connString == "" {
		return fmt.Errorf("no connection string (pass one or set $DATABASE_URL)")
	}
	f.ashHz = 0 // no wait sampling needed for tuning
	f.noStore = true

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
	var tuning []model.Finding
	for _, fd := range c.Findings {
		if findings.TuningIDs[fd.ID] {
			tuning = append(tuning, fd)
		}
	}

	fmt.Printf("%s · %s · %d tuning recommendation(s)\n\n", st.Head(host), pgVersionShort(c.Server.VersionNum), len(tuning))
	if len(tuning) == 0 {
		fmt.Println(st.Good("✓ no configuration changes recommended for the observed workload"))
		return nil
	}
	for _, fd := range tuning {
		fmt.Printf("%s %s\n", st.Warn("●"), st.Warn(fd.Title))
		fmt.Printf("  %s\n", st.Dim(fd.Detail))
		fmt.Printf("  %s %s\n\n", st.Good("→"), fd.Remediation)
	}
	fmt.Println(st.Dim("pgbot recommends; it never changes settings. Apply what fits your workload."))
	return nil
}

func pgVersionShort(num int) string {
	if num == 0 {
		return "postgres"
	}
	return fmt.Sprintf("postgres %d.%d", num/10000, num%100)
}
