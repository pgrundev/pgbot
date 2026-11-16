package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/schemasnap"
	"github.com/spf13/cobra"
)

// newSchemaCmd — `pgbot schema`: catalog snapshots for migration review.
//
//	pgbot schema snapshot [conn] > before.json     (read-only, catalog only)
//	pgbot schema diff before.json after.json       (offline: what the migration did)
//	pgbot schema compare a.json b.json             (offline: two servers, same migration)
func newSchemaCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "schema",
		Short: "Schema snapshots for migration review: snapshot, diff (what a migration did), compare (two servers)",
		Long: "Catalog-only schema snapshots. `snapshot` reads pg_catalog (never table data) and prints JSON.\n" +
			"`diff` compares a snapshot taken before a migration with one taken after it on the SAME database and\n" +
			"flags what is dangerous to run in production: table rewrites, blocking index builds, validating scans,\n" +
			"dropped tables/columns, renames. `compare` compares the same migration applied on two DIFFERENT servers\n" +
			"(for example PGX and PostgreSQL) and lists every structural difference. diff and compare are offline.",
	}
	cmd.AddCommand(newSchemaSnapshotCmd(), newSchemaDiffCmd(), newSchemaCompareCmd())
	return cmd
}

func newSchemaSnapshotCmd() *cobra.Command {
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "snapshot [connection-string]",
		Short: "Print the schema structure as JSON (catalog only, no table data)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			connString := firstNonEmpty(argAt(args, 0), os.Getenv("DATABASE_URL"), os.Getenv("PGBOT_DATABASE_URL"), pgServiceFallback())
			if connString == "" {
				return fmt.Errorf("no connection string (pass one or set $DATABASE_URL)")
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			target, err := conn.Connect(ctx, connString)
			if err != nil {
				return err
			}
			defer target.Close()
			snap, err := schemasnap.Take(ctx, target.Pool)
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), snap)
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Second, "wall-clock budget")
	return cmd
}

func newSchemaDiffCmd() *cobra.Command {
	var format, failOn string
	var largeBytes int64
	cmd := &cobra.Command{
		Use:   "diff <before.json> <after.json>",
		Short: "What a migration did, from snapshots before and after it (offline)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			before, err := readSnapshot(args[0])
			if err != nil {
				return err
			}
			after, err := readSnapshot(args[1])
			if err != nil {
				return err
			}
			if failOn != "none" && failOn != "warning" && failOn != "warn" && failOn != "critical" {
				return usageErrf("unknown --fail-on %q (valid: critical, warning, none)", failOn)
			}
			rep := schemasnap.Diff(before, after, schemasnap.DiffOptions{LargeTableBytes: largeBytes})
			switch format {
			case "json":
				if err := writeJSON(cmd.OutOrStdout(), rep); err != nil {
					return err
				}
			case "text", "":
				writeDiffText(cmd.OutOrStdout(), rep)
			default:
				return usageErrf("unknown --format %q (valid: text, json)", format)
			}
			if code := diffExitCode(rep, failOn); code != 0 {
				os.Exit(code)
			}
			return nil
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&format, "format", "text", "output format: text|json")
	fl.StringVar(&failOn, "fail-on", "none", "exit 1 (warning) or 2 (critical) when a finding at/above this severity exists: critical|warning|none")
	fl.Int64Var(&largeBytes, "large-table-bytes", schemasnap.DefaultLargeTableBytes, "a rewrite/blocking build/validating scan on a table at least this large is critical")
	return cmd
}

func newSchemaCompareCmd() *cobra.Command {
	var format, aLabel, bLabel string
	var failOnDifference bool
	cmd := &cobra.Command{
		Use:   "compare <a.json> <b.json>",
		Short: "Structural differences between two servers' snapshots of the same schema (offline)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := readSnapshot(args[0])
			if err != nil {
				return err
			}
			b, err := readSnapshot(args[1])
			if err != nil {
				return err
			}
			rep := schemasnap.Compare(a, b, aLabel, bLabel)
			switch format {
			case "json":
				if err := writeJSON(cmd.OutOrStdout(), rep); err != nil {
					return err
				}
			case "text", "":
				w := cmd.OutOrStdout()
				if rep.Identical {
					fmt.Fprintf(w, "identical structure: %s (%s) and %s (%s)\n", aLabel, rep.AServer, bLabel, rep.BServer)
				} else {
					fmt.Fprintf(w, "%d structural differences between %s and %s:\n", len(rep.Differences), aLabel, bLabel)
					for _, d := range rep.Differences {
						fmt.Fprintf(w, "  %s [%s]\n    %s: %s\n    %s: %s\n", d.Object, d.Field, aLabel, orMissing(d.A), bLabel, orMissing(d.B))
					}
				}
			default:
				return usageErrf("unknown --format %q (valid: text, json)", format)
			}
			if failOnDifference && !rep.Identical {
				os.Exit(1)
			}
			return nil
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&format, "format", "text", "output format: text|json")
	fl.StringVar(&aLabel, "a-label", "a", "name for the first snapshot's server")
	fl.StringVar(&bLabel, "b-label", "b", "name for the second snapshot's server")
	fl.BoolVar(&failOnDifference, "fail-on-difference", false, "exit 1 when the structures differ")
	return cmd
}

func orMissing(s string) string {
	if s == "" {
		return "(missing)"
	}
	return s
}

func readSnapshot(path string) (*schemasnap.Snapshot, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s schemasnap.Snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("%s: not a schema snapshot: %w", path, err)
	}
	if s.Kind != schemasnap.SnapshotKind {
		return nil, fmt.Errorf("%s: not a schema snapshot (kind %q)", path, s.Kind)
	}
	return &s, nil
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func diffExitCode(rep schemasnap.DiffReport, failOn string) int {
	switch failOn {
	case "critical":
		if rep.Summary.Critical > 0 {
			return 2
		}
	case "warning", "warn":
		if rep.Summary.Critical > 0 {
			return 2
		}
		if rep.Summary.Warning > 0 {
			return 1
		}
	}
	return 0
}

func writeDiffText(w io.Writer, rep schemasnap.DiffReport) {
	if len(rep.Findings) == 0 {
		fmt.Fprintln(w, "no schema changes")
		return
	}
	fmt.Fprintf(w, "migration review: %d critical · %d warning · %d info\n", rep.Summary.Critical, rep.Summary.Warning, rep.Summary.Info)
	for _, f := range rep.Findings {
		fmt.Fprintf(w, "\n%-8s %s  %s\n  %s\n", f.Severity, f.ID, f.Object, f.Detail)
		if f.Advice != "" {
			fmt.Fprintf(w, "  → %s\n", f.Advice)
		}
	}
	fmt.Fprintf(w, "\n%s\n", rep.Caveat)
}
