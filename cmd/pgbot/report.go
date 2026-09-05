package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/pgrundev/pgbot/internal/render"
	"github.com/pgrundev/pgbot/internal/report"
	"github.com/spf13/cobra"
)

// newReportCmd — `pgbot report`. The full inspection as one self-contained
// HTML page: findings with severities and caveats, top queries, tables,
// indexes, waits, settings — the file a DBA attaches to the ticket. Same
// collection pipeline as inspect (a snapshot is stored as usual), rendered
// for a browser instead of a terminal.
func newReportCmd() *cobra.Command {
	var f inspectFlags
	cmd := &cobra.Command{
		Use:   "report <connection-string>",
		Short: "Full inspection as one self-contained HTML page: pgbot report > report.html",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			connString := firstNonEmpty(argAt(args, 0), os.Getenv("DATABASE_URL"), os.Getenv("PGBOT_DATABASE_URL"))
			if connString == "" {
				return fmt.Errorf("no connection string (pass one or set $DATABASE_URL)")
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), f.timeout)
			defer cancel()

			c, _, err := gather(ctx, connString, f)
			if err != nil {
				return err
			}
			fmt.Print(report.Render(c, render.HealthScore(c), version))
			return nil
		},
	}
	fl := cmd.Flags()
	fl.DurationVar(&f.interval, "interval", time.Second, "gap between the two counter samples")
	fl.IntVar(&f.ashHz, "ash-hz", 10, "wait sampling rate (0 disables the waits section)")
	fl.DurationVar(&f.window, "window", 5*time.Second, "wait sampling window")
	fl.BoolVar(&f.noStore, "no-store", false, "don't store this run as a baseline snapshot")
	fl.DurationVar(&f.timeout, "timeout", 60*time.Second, "total wall-clock budget")
	return cmd
}
