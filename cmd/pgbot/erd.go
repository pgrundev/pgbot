package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/erd"
	"github.com/spf13/cobra"
)

// newERDCmd — `pgbot erd`. The schema as an entity-relationship diagram,
// drawn in the terminal: table boxes with PK/FK markers plus a crow's-foot
// relationship forest. --mermaid emits the pasteable erDiagram instead.
// Structure only, never data — and the connection string never leaves the
// machine, unlike paste-your-DSN diagram websites.
func newERDCmd() *cobra.Command {
	var schemaFilter, layout string
	var mermaid, htmlOut bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "erd <connection-string>",
		Short: "Draw the schema as an ER diagram in the terminal (--mermaid for GitHub/mermaid.live)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			connString := firstNonEmpty(argAt(args, 0), os.Getenv("DATABASE_URL"), os.Getenv("PGBOT_DATABASE_URL"))
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

			s, err := erd.Introspect(ctx, target.Pool, schemaFilter)
			if err != nil {
				return err
			}
			s.Info.Version = pgVersionShort(target.Caps.VersionNum)
			switch {
			case htmlOut:
				fmt.Print(erd.RenderHTML(s))
			case mermaid:
				fmt.Print(erd.RenderMermaid(s))
			case layout == "row":
				fmt.Print(erd.RenderASCIIRow(s))
			case layout == "column" || layout == "":
				fmt.Print(erd.RenderASCII(s, false))
			default:
				return usageErrf("unknown --layout %q (valid: column, row)", layout)
			}
			return nil
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&schemaFilter, "schema", "", "limit to one schema (default: every user schema)")
	fl.StringVar(&layout, "layout", "column", "diagram direction: column (top-down) or row (left-to-right, dashed edges)")
	fl.BoolVar(&mermaid, "mermaid", false, "emit a mermaid erDiagram instead of the terminal view")
	fl.BoolVar(&htmlOut, "html", false, "emit a self-contained interactive HTML diagram (pan/zoom, no external requests): pgbot erd --html > schema.html")
	fl.DurationVar(&timeout, "timeout", 30*time.Second, "wall-clock budget")
	return cmd
}
