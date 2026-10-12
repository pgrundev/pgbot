package render

import (
	"fmt"
	"io"
	"strings"

	"github.com/pgrundev/pgbot/internal/model"
)

// Prometheus writes findings and key gauges for one database in the node_exporter
// textfile format. See PrometheusAll for the format contract.
func Prometheus(w io.Writer, c *model.Context) error {
	return PrometheusAll(w, []*model.Context{c})
}

// PrometheusAll writes a single, valid node_exporter textfile exposition covering
// every database in contexts (one entry under --all-databases, N under it). It is
// meant for a textfile collector driven by a cron or systemd timer — pgbot stays
// agentless; there is no daemon. Every finding is one pgbot_finding series; a
// SUPPRESSED finding is exported with suppressed="true" rather than omitted, so a
// metrics consumer sees what a config muted (B6).
//
// The text format forbids a repeated # HELP/# TYPE line for the same metric name,
// so headers are emitted ONCE per family with every database's samples grouped
// beneath — not one self-contained block per database, which would duplicate them
// and make promtool (and the textfile collector) reject the file.
func PrometheusAll(w io.Writer, contexts []*model.Context) error {
	finding := &promFamily{name: "pgbot_finding", typ: "gauge",
		help: `A pgbot finding (1 = present). suppressed="true" means muted by .pgbot.toml; destructive="true" means it carries a safety guard against a destructive action.`}
	total := &promFamily{name: "pgbot_findings_total", typ: "gauge",
		help: "Active (unsuppressed) findings by severity."}

	// Gauges are pre-declared in a fixed order so output is deterministic; a family
	// with no samples across any database is dropped (no empty header).
	cacheHit := &promFamily{name: "pgbot_cache_hit_ratio", typ: "gauge", help: "Buffer cache hit ratio (0-1)."}
	rollback := &promFamily{name: "pgbot_rollback_ratio", typ: "gauge", help: "Transaction rollback ratio (0-1)."}
	tps := &promFamily{name: "pgbot_tps", typ: "gauge", help: "Transactions per second."}
	xidAge := &promFamily{name: "pgbot_xid_age", typ: "gauge", help: "Oldest transaction-id age (toward the ~2.1B wraparound wall)."}
	mxidAge := &promFamily{name: "pgbot_mxid_age", typ: "gauge", help: "Oldest multixact-id age."}
	connUsed := &promFamily{name: "pgbot_connections_used", typ: "gauge", help: "Current connections."}
	connMax := &promFamily{name: "pgbot_connections_max", typ: "gauge", help: "max_connections."}
	walBytes := &promFamily{name: "pgbot_wal_dir_bytes", typ: "gauge", help: "pg_wal directory size in bytes."}
	replicaLag := &promFamily{name: "pgbot_replica_lag_seconds", typ: "gauge", help: "Replica replay lag in seconds."}

	families := []*promFamily{
		finding, total, cacheHit, rollback, tps, xidAge, mxidAge, connUsed, connMax, walBytes, replicaLag,
	}

	for _, c := range contexts {
		db := promScope(c)
		counts := map[string]int{}
		for _, f := range c.Findings {
			// Preexisting findings (--fail-on-new) don't count: the exit code
			// passes them (inspect.go), so an alert on pgbot_findings_total must
			// not page on a run CI declared clean. They stay visible as series
			// with preexisting="true", the label alerting can silence by.
			if !f.Suppressed && !f.Preexisting {
				counts[f.Severity]++
			}
			finding.add(fmt.Sprintf("pgbot_finding{%s,id=\"%s\",severity=\"%s\",dimension=\"%s\",object=\"%s\",suppressed=\"%s\",preexisting=\"%s\",destructive=\"%s\"} 1",
				db, esc(f.ID), esc(f.Severity), esc(f.Impact.Dimension), esc(f.Object), boolLabel(f.Suppressed),
				boolLabel(f.Preexisting),
				boolLabel(f.Safety != nil && len(f.Safety.BlockingCaveats) > 0)))
		}
		for _, sev := range []string{"critical", "warn", "info"} {
			total.add(fmt.Sprintf("pgbot_findings_total{%s,severity=\"%s\"} %d", db, sev, counts[sev]))
		}

		// Underlying gauges — the numbers behind the findings, so alerts can trigger
		// on a trend before a finding crosses its threshold.
		if h := c.Health; h != nil {
			cacheHit.gauge(db, h.CacheHitRatio)
			rollback.gauge(db, h.RollbackRatio)
			tps.gauge(db, h.TPS)
		}
		if l := c.Limits; l != nil {
			xidAge.set(db, float64(l.MaxXIDAge))
			mxidAge.set(db, float64(l.MaxMXIDAge))
			if l.ConnectionsMax > 0 {
				connUsed.set(db, float64(l.ConnectionsUsed))
				connMax.set(db, float64(l.ConnectionsMax))
			}
		}
		if c.WAL != nil && c.WAL.DirBytes != nil {
			walBytes.set(db, float64(*c.WAL.DirBytes))
		}
		if r := c.Replication; r != nil {
			for _, rep := range r.Replicas {
				if rep.ReplayLagSec != nil {
					name := rep.AppName
					if name == "" {
						name = rep.ClientAddr
					}
					replicaLag.add(fmt.Sprintf("pgbot_replica_lag_seconds{%s,replica=\"%s\"} %g", db, esc(name), *rep.ReplayLagSec))
				}
			}
		}
	}

	var b strings.Builder
	for _, f := range families {
		f.write(&b)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// promFamily accumulates the samples of one metric family so its # HELP/# TYPE
// header can be written exactly once, ahead of every sample.
type promFamily struct {
	name, help, typ string
	samples         []string
}

func (f *promFamily) add(sample string) { f.samples = append(f.samples, sample) }

// set writes one plain scope-labelled gauge sample. scope arrives already
// rendered by promScope (values escaped by esc; wrapping them in %q would escape
// them a second time).
func (f *promFamily) set(scope string, v float64) {
	f.add(fmt.Sprintf("%s{%s} %g", f.name, scope, v))
}

// promScope renders the labels that identify one Context's series: the database,
// plus the cluster member under --all-instances — without it, two instances'
// samples for the same database would be duplicate series and the textfile
// collector would reject the file. Single-instance output is unchanged.
func promScope(c *model.Context) string {
	scope := fmt.Sprintf("database=\"%s\"", esc(c.Server.Database))
	if c.Server.Instance != "" {
		scope += fmt.Sprintf(",instance=\"%s\",role=\"%s\"", esc(c.Server.Instance), esc(c.Server.InstanceRole))
	}
	return scope
}

// gauge writes a sample from an optional float pointer (skipped when nil).
func (f *promFamily) gauge(db string, v *float64) {
	if v != nil {
		f.set(db, *v)
	}
}

func (f *promFamily) write(b *strings.Builder) {
	if len(f.samples) == 0 {
		return
	}
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", f.name, f.help, f.name, f.typ)
	for _, s := range f.samples {
		b.WriteString(s)
		b.WriteByte('\n')
	}
}

// esc escapes a Prometheus label value: backslash, quote, newline.
func esc(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}

func boolLabel(v bool) string {
	if v {
		return "true"
	}
	return "false"
}
