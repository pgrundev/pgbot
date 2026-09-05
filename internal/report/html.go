// Package report renders a full inspect Context as one self-contained HTML
// page — the DBA report: findings with severities and caveats, every top
// query, tables, indexes, vacuum, settings, waits. The Context is PII-free by
// construction and the page makes zero external requests, so the report is
// safe to file, mail, or attach to a ticket.
package report

import (
	"fmt"
	"html"
	"sort"
	"strings"

	"github.com/pgrundev/pgbot/internal/model"
	"github.com/pgrundev/pgbot/internal/render"
)

type page struct{ b strings.Builder }

func (p *page) w(format string, a ...any) { fmt.Fprintf(&p.b, format, a...) }
func (p *page) raw(s string)              { p.b.WriteString(s) }
func esc(s string) string                 { return html.EscapeString(s) }
func pct(v *float64) string {
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%.1f%%", *v*100)
}
func num(v *float64, unit string) string {
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%.1f%s", *v, unit)
}

// section opens a nav-linked block; returns its close tag via the caller's defer.
func (p *page) section(id, title string) {
	p.w(`<section id="%s"><h2>%s</h2>`, id, esc(title))
}

// table renders a sortable table; cells are pre-escaped by the caller only
// where noted, otherwise escaped here.
func (p *page) table(headers []string, rows [][]string) {
	if len(rows) == 0 {
		return
	}
	p.raw(`<div class="tblwrap"><table><thead><tr>`)
	for _, h := range headers {
		p.w(`<th>%s</th>`, esc(h))
	}
	p.raw(`</tr></thead><tbody>`)
	for _, r := range rows {
		p.raw(`<tr>`)
		for _, c := range r {
			p.w(`<td>%s</td>`, esc(c))
		}
		p.raw(`</tr>`)
	}
	p.raw(`</tbody></table></div>`)
}

// Render builds the report from a computed Context. score is the same 0–100
// grade the terminal dashboard shows; version stamps which pgbot produced it.
func Render(c *model.Context, score int, version string) string {
	p := &page{}
	nav := []string{}
	addNav := func(id, label string) { nav = append(nav, fmt.Sprintf(`<a href="#%s">%s</a>`, id, esc(label))) }

	body := &page{}

	// --- Findings ---
	if len(c.Findings) > 0 {
		addNav("findings", "Findings")
		body.section("findings", "Findings")
		fs := append([]model.Finding(nil), c.Findings...)
		order := map[string]int{"critical": 0, "warning": 1, "info": 2}
		sort.SliceStable(fs, func(i, j int) bool { return order[fs[i].Severity] < order[fs[j].Severity] })
		for _, f := range fs {
			cls := f.Severity
			if f.Suppressed {
				cls += " suppressed"
			}
			body.w(`<div class="finding %s"><div class="sev">%s</div><div class="fbody"><b>%s</b>`,
				esc(cls), esc(strings.ToUpper(f.Severity)), esc(f.Title))
			if f.Detail != "" {
				body.w(`<p>%s</p>`, esc(f.Detail))
			}
			for _, e := range f.Evidence {
				body.w(`<div class="evi">%s</div>`, esc(e))
			}
			if f.Remediation != "" {
				body.w(`<p class="fix">Fix: %s</p>`, esc(f.Remediation))
			}
			for _, cv := range f.Caveats {
				body.w(`<p class="caveat">but: %s</p>`, esc(cv))
			}
			if f.Suppressed {
				body.w(`<p class="caveat">suppressed: %s</p>`, esc(f.SuppressionReason))
			}
			body.raw(`</div></div>`)
		}
		body.raw(`</section>`)
	}

	// --- Queries ---
	if c.Queries != nil && len(c.Queries.Top) > 0 {
		addNav("queries", "Queries")
		body.section("queries", "Top queries (pg_stat_statements, cumulative)")
		var rows [][]string
		for _, q := range c.Queries.Top {
			share := ""
			if c.Queries.TotalExecMS > 0 {
				share = fmt.Sprintf("%.1f%%", q.TotalMS/c.Queries.TotalExecMS*100)
			}
			rows = append(rows, []string{
				fmt.Sprintf("%.0f ms", q.TotalMS), share, fmt.Sprintf("%d", q.Calls),
				fmt.Sprintf("%.2f ms", q.MeanMS), fmt.Sprintf("%.0f ms", q.MaxMS),
				fmt.Sprintf("%d", q.Rows), pct(q.CacheHit), q.Query,
			})
		}
		body.table([]string{"total", "share", "calls", "mean", "max", "rows", "cache", "query"}, rows)
		body.raw(`</section>`)
	}

	// --- Tables ---
	if c.Tables != nil && len(c.Tables.Top) > 0 {
		addNav("tables", "Tables")
		body.section("tables", "Largest tables")
		var rows [][]string
		for _, t := range c.Tables.Top {
			rows = append(rows, []string{
				t.Schema + "." + t.Name, render.HumanBytes(t.TotalBytes),
				fmt.Sprintf("%d", t.LiveTuples), fmt.Sprintf("%d", t.DeadTuples),
				fmt.Sprintf("%.1f%%", t.DeadRatio*100),
				fmt.Sprintf("%d", t.SeqScans), fmt.Sprintf("%d", t.IndexScans),
			})
		}
		body.table([]string{"table", "size", "live", "dead", "dead%", "seq scans", "idx scans"}, rows)
		body.raw(`</section>`)
	}

	// --- Indexes ---
	if c.Indexes != nil && (len(c.Indexes.Unused)+len(c.Indexes.Redundant)+len(c.Indexes.UnindexedFKs) > 0) {
		addNav("indexes", "Indexes")
		body.section("indexes", fmt.Sprintf("Indexes (%d total)", c.Indexes.Total))
		if len(c.Indexes.Unused) > 0 {
			body.raw(`<h3>Zero-scan (per-node counters — a replica may still use them)</h3>`)
			var rows [][]string
			for _, ix := range c.Indexes.Unused {
				rows = append(rows, []string{ix.Schema + "." + ix.Table, ix.Name,
					render.HumanBytes(ix.Bytes), ix.Definition})
			}
			body.table([]string{"table", "index", "size", "definition"}, rows)
		}
		if len(c.Indexes.Redundant) > 0 {
			body.raw(`<h3>Redundant (leading prefix of another index)</h3>`)
			var rows [][]string
			for _, ix := range c.Indexes.Redundant {
				rows = append(rows, []string{ix.Schema + "." + ix.Table, ix.Name, "covered by " + ix.CoveredBy, render.HumanBytes(ix.Bytes)})
			}
			body.table([]string{"table", "index", "note", "size"}, rows)
		}
		if len(c.Indexes.UnindexedFKs) > 0 {
			body.raw(`<h3>Foreign keys with no supporting index</h3>`)
			var rows [][]string
			for _, fk := range c.Indexes.UnindexedFKs {
				rows = append(rows, []string{fk.Schema + "." + fk.Table, fk.Constraint, fk.Columns, render.HumanBytes(fk.ChildBytes)})
			}
			body.table([]string{"table", "constraint", "columns", "child size"}, rows)
		}
		body.raw(`</section>`)
	}

	// --- Wait profile ---
	if wp := c.WaitProfile; wp != nil && wp.Available && len(wp.Buckets) > 0 {
		addNav("waits", "Waits")
		body.section("waits", fmt.Sprintf("Wait profile (sampled, %d samples over %.1fs)", wp.Samples, wp.WindowSeconds))
		var rows [][]string
		for _, bk := range wp.Buckets {
			evs := []string{}
			for _, e := range bk.Events {
				evs = append(evs, fmt.Sprintf("%s %.0f%%", e.Event, e.Share*100))
			}
			rows = append(rows, []string{bk.Type, fmt.Sprintf("%.0f%%", bk.Share*100), strings.Join(evs, " · ")})
		}
		body.table([]string{"class", "share", "events"}, rows)
		body.raw(`</section>`)
	}

	// --- Settings ---
	if c.Settings != nil && len(c.Settings.Overrides) > 0 {
		addNav("settings", "Settings")
		body.section("settings", "Non-default settings")
		keys := make([]string, 0, len(c.Settings.Overrides))
		for k := range c.Settings.Overrides {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var rows [][]string
		for _, k := range keys {
			rows = append(rows, []string{k, c.Settings.Overrides[k]})
		}
		body.table([]string{"setting", "value"}, rows)
		body.raw(`</section>`)
	}

	// --- Header assembly ---
	dbsize := ""
	if c.Tables != nil && c.Tables.DBSizeBytes > 0 {
		dbsize = render.HumanBytes(c.Tables.DBSizeBytes)
	}
	stats := []string{}
	if c.Health != nil {
		stats = append(stats, fmt.Sprintf("%d connections", c.Health.Connections))
		if s := num(c.Health.TPS, " tps"); s != "" {
			stats = append(stats, s)
		}
		if c.Health.CacheHitUsable() {
			stats = append(stats, "cache hit "+pct(c.Health.CacheHitRatio))
		}
	}
	if dbsize != "" {
		stats = append(stats, dbsize)
	}

	p.raw(`<!doctype html><html><head><meta charset="utf-8">
<title>` + esc(c.Server.Database) + ` — pgbot report</title>
<style>
  :root{--bg:#0b0d0f;--panel:#14171b;--line:#262c33;--fg:#dfe4ea;--muted:#8b95a3;--dim:#5a6472;
        --accent:#5fb0e6;--green:#5ecf9a;--orange:#e8a75a;--red:#e06c6c}
  body{margin:0;background:var(--bg);color:var(--fg);
       font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:14px;line-height:1.5}
  header{padding:1.4rem 2rem;border-bottom:1px solid var(--line)}
  header h1{margin:0;font-size:1.2rem}
  header .meta{color:var(--muted);margin-top:.3rem}
  header .score{float:right;font-size:2rem;font-weight:700;color:var(--green)}
  nav{position:sticky;top:0;background:var(--bg);border-bottom:1px solid var(--line);
      padding:.5rem 2rem;display:flex;gap:1.2rem;flex-wrap:wrap;z-index:2}
  nav a{color:var(--accent);text-decoration:none;font-size:.85rem}
  main{padding:1rem 2rem 4rem;max-width:1200px}
  h2{font-size:1rem;color:var(--accent);border-bottom:1px solid var(--line);padding-bottom:.4rem;margin-top:2.2rem}
  h3{font-size:.85rem;color:var(--muted);margin:1.2rem 0 .4rem}
  .tblwrap{overflow-x:auto}
  table{border-collapse:collapse;width:100%%;font-size:.82rem}
  th{text-align:left;color:var(--dim);font-weight:600;padding:.35rem .7rem .35rem 0;cursor:pointer;
     border-bottom:1px solid var(--line);white-space:nowrap}
  td{padding:.3rem .7rem .3rem 0;border-bottom:1px solid #1a1f24;vertical-align:top;
     font-variant-numeric:tabular-nums}
  .finding{display:flex;gap:1rem;border:1px solid var(--line);border-radius:6px;
           background:var(--panel);padding:.8rem 1rem;margin:.6rem 0}
  .finding .sev{font-weight:700;font-size:.72rem;letter-spacing:.08em;min-width:5.5rem}
  .finding.critical .sev{color:var(--red)} .finding.warning .sev{color:var(--orange)}
  .finding.info .sev{color:var(--muted)} .finding.suppressed{opacity:.55}
  .fbody p{margin:.3rem 0;color:var(--muted)} .fbody b{color:var(--fg)}
  .evi{color:var(--muted);font-size:.82rem;padding-left:.8rem}
  .fix{color:var(--green)} .caveat{color:var(--orange);font-size:.82rem}
  footer{padding:1rem 2rem;color:var(--dim);border-top:1px solid var(--line);font-size:.8rem}
</style></head><body>
<header><span class="score">` + fmt.Sprintf("%d", score) + `<span style="font-size:.9rem;color:var(--dim)">/100</span></span>
<h1>` + esc(c.Server.Database) + ` — pgbot report</h1>
<div class="meta">` + esc(c.Server.VersionText) + ` · ` + esc(strings.Join(stats, " · ")) + `</div>
</header>
<nav>` + strings.Join(nav, "") + `</nav>
<main>`)
	p.raw(body.b.String())
	p.raw(`</main>
<footer>generated by pgbot ` + esc(version) + ` · every value carries its exactness (sampled rates, cumulative totals, point-in-time reads) ·
query text normalized/scrubbed — this file is PII-free and makes no external requests</footer>
<script>
document.querySelectorAll('th').forEach(function(th){
  th.addEventListener('click',function(){
    var tb=th.closest('table').tBodies[0],i=th.cellIndex,asc=th.asc=!th.asc;
    Array.prototype.slice.call(tb.rows).sort(function(a,b){
      var x=a.cells[i].textContent,y=b.cells[i].textContent,
          nx=parseFloat(x.replace(/[^0-9.\-]/g,'')),ny=parseFloat(y.replace(/[^0-9.\-]/g,''));
      var c=(!isNaN(nx)&&!isNaN(ny))?nx-ny:x.localeCompare(y);
      return asc?c:-c;
    }).forEach(function(r){tb.appendChild(r)});
  });
});
</script></body></html>
`)
	return p.b.String()
}
