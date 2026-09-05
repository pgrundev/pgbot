package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/render"
	"github.com/spf13/cobra"
)

// activityRow is one live session from pg_stat_activity.
type activityRow struct {
	PID       int     `db:"pid" json:"pid"`
	User      string  `db:"usename" json:"user,omitempty"`
	DB        string  `db:"datname" json:"db,omitempty"`
	App       string  `db:"app_name" json:"app,omitempty"`
	State     string  `db:"state" json:"state"`
	Wait      string  `db:"wait" json:"wait,omitempty"`
	XactAgeS  float64 `db:"xact_age_s" json:"xact_age_s,omitempty"`
	QueryAgeS float64 `db:"query_age_s" json:"query_age_s,omitempty"`
	Query     string  `db:"query" json:"query,omitempty"`
}

const activitySQL = `
SELECT pid,
       coalesce(usename::text, '')        AS usename,
       coalesce(datname::text, '')        AS datname,
       coalesce(application_name, '')     AS app_name,
       coalesce(state, '')                AS state,
       CASE WHEN wait_event IS NULL THEN ''
            ELSE coalesce(wait_event_type, '') || ':' || wait_event END AS wait,
       coalesce(extract(epoch FROM now() - xact_start), 0)  AS xact_age_s,
       coalesce(extract(epoch FROM now() - query_start), 0) AS query_age_s,
       left(coalesce(query, ''), 300)     AS query
FROM pg_stat_activity
WHERE backend_type = 'client backend' AND pid <> pg_backend_pid()
ORDER BY CASE state WHEN 'active' THEN 0
                    WHEN 'idle in transaction' THEN 1
                    WHEN 'idle in transaction (aborted)' THEN 1
                    ELSE 2 END,
         query_start ASC NULLS LAST, pid`

// newActivityCmd — `pgbot activity`. The live sessions right now: PIDs, ages,
// waits, and what each one is running. pg_stat_activity for humans — pgbot's
// own connections excluded, query text scrubbed unless the operator opts out.
func newActivityCmd() *cobra.Command {
	var all, jsonOut, rawQueries, noColor bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "activity <connection-string>",
		Short: "Live sessions right now — PIDs, ages, waits, and what each is running",
		Long: `Lists the current client backends from pg_stat_activity: who is connected,
what state they're in, how long their transaction and query have run, what they
wait on, and the (scrubbed) SQL. Plain idle sessions are summarized, not listed
(--all lists them too). pgbot's own connections are excluded by PID.`,
		Args: cobra.MaximumNArgs(1),
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
			target.Warm(ctx)

			rows, err := target.Pool.Query(ctx, target.ExcludeSelf(activitySQL))
			if err != nil {
				return err
			}
			got, err := pgx.CollectRows(rows, pgx.RowToStructByNameLax[activityRow])
			if err != nil {
				return err
			}

			if jsonOut {
				for _, r := range got {
					if keepActivityRow(r.State, all) {
						fmt.Println(activityJSONLine(r))
					}
				}
				return nil
			}
			renderActivity(got, all, rawQueries, noColor)
			return nil
		},
	}
	fl := cmd.Flags()
	fl.BoolVar(&all, "all", false, "also list plain idle sessions (default: summarized)")
	fl.BoolVar(&jsonOut, "json", false, "one JSON object per session (scrubbed)")
	fl.BoolVar(&rawQueries, "raw-query-text", false, "show query text verbatim instead of scrubbed (terminal use)")
	fl.BoolVar(&noColor, "no-color", false, "disable ANSI color")
	fl.DurationVar(&timeout, "timeout", 30*time.Second, "wall-clock budget")
	return cmd
}

// keepActivityRow: active, idle-in-transaction (either kind), and anything
// waiting always shows; plain idle only under --all.
func keepActivityRow(state string, all bool) bool {
	if all {
		return true
	}
	return state != "idle" && state != ""
}

// fmtAgeShort renders a duration the way an operator reads one: 43s, 1m30s,
// 1h1m, 1d1h. Zero renders empty — the column stays quiet when there is
// nothing to say.
func fmtAgeShort(sec float64) string {
	if sec < 0.5 {
		return ""
	}
	d := time.Duration(sec * float64(time.Second)).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		m := int(d.Minutes())
		s := int(d.Seconds()) - m*60
		if s == 0 {
			return fmt.Sprintf("%dm", m)
		}
		return fmt.Sprintf("%dm%ds", m, s)
	case d < 24*time.Hour:
		h := int(d.Hours())
		m := int(d.Minutes()) - h*60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%dm", h, m)
	default:
		days := int(d.Hours()) / 24
		h := int(d.Hours()) - days*24
		if h == 0 {
			return fmt.Sprintf("%dd", days)
		}
		return fmt.Sprintf("%dd%dh", days, h)
	}
}

// activityJSONLine is the scrubbed machine row.
func activityJSONLine(r activityRow) string {
	r.Query = conn.ScrubQueryText(r.Query)
	b, err := json.Marshal(r)
	if err != nil {
		return `{"error":"encode failed"}`
	}
	return string(b)
}

func renderActivity(rows []activityRow, all, rawQueries, noColor bool) {
	st := render.NewStyler(useColor(noColor))
	var shown []activityRow
	idle := 0
	for _, r := range rows {
		if keepActivityRow(r.State, all) {
			shown = append(shown, r)
		} else {
			idle++
		}
	}
	fmt.Printf("%s · %d session(s)", st.Head("pgbot activity"), len(rows))
	if idle > 0 && !all {
		fmt.Printf(" · %s", st.Dim(fmt.Sprintf("%d idle hidden (--all shows them)", idle)))
	}
	fmt.Println()
	fmt.Println()
	if len(shown) == 0 {
		fmt.Println(st.Dim("nothing active right now"))
		return
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "  pid\tuser@db\tapp\tstate\twait\txact\trunning\tquery")
	for _, r := range shown {
		q := r.Query
		if !rawQueries {
			q = conn.ScrubQueryText(q)
		}
		state := r.State
		switch r.State {
		case "active":
			state = st.Good(state)
		case "idle in transaction", "idle in transaction (aborted)":
			state = st.Warn(state)
		}
		wait := r.Wait
		if wait != "" {
			wait = st.Warn(wait)
		}
		fmt.Fprintf(tw, "  %d\t%s@%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.PID, r.User, r.DB, truncStr(r.App, 16), state, wait,
			fmtAgeShort(r.XactAgeS), fmtAgeShort(r.QueryAgeS), truncStr(q, 60))
	}
	tw.Flush()
	fmt.Println()
	fmt.Println(st.Dim("xact = transaction age · running = current query age · text scrubbed (--raw-query-text for verbatim)"))
}
