package erd

import (
	"fmt"
	"html"
	"sort"
	"strings"
)

// RenderHTML emits one fully self-contained HTML file: an inline SVG diagram
// (table boxes, dashed relationship edges with arrowheads into the parent)
// plus ~40 lines of inline pan/zoom script. No CDN, no external fonts, no
// requests of any kind — the schema never leaves the file, which is the whole
// point of running the diagram tool locally.
func RenderHTML(s Schema) string {
	const (
		charW, rowH    = 8.5, 22.0
		padX, padY     = 12.0, 8.0
		titleH         = 30.0
		colGap, boxGap = 90.0, 28.0
	)

	tables := append([]Table(nil), s.Tables...)
	sort.Slice(tables, func(i, j int) bool { return tables[i].Name < tables[j].Name })

	// Same layered layout as --layout row: parents left, children right.
	depth := map[string]int{}
	known := map[string]bool{}
	for _, t := range tables {
		depth[t.Name], known[t.Name] = 0, true
	}
	for range tables {
		changed := false
		for _, e := range s.Edges {
			if known[e.FromTable] && known[e.ToTable] && depth[e.FromTable] < depth[e.ToTable]+1 {
				depth[e.FromTable] = depth[e.ToTable] + 1
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	maxDepth := 0
	for _, d := range depth {
		maxDepth = maxInt(maxDepth, d)
	}

	type box struct {
		x, y, w, h float64
		fkY        map[string]float64 // FK column name → row center y
	}
	boxes := map[string]*box{}
	colX := 0.0
	for d := 0; d <= maxDepth; d++ {
		colW, y := 0.0, 0.0
		var col []*Table
		for i := range tables {
			if depth[tables[i].Name] == d {
				col = append(col, &tables[i])
			}
		}
		for _, t := range col {
			w := float64(len(t.Schema+"."+t.Name))*charW + 2*padX
			for _, c := range t.Columns {
				lw := float64(len(c.Name)+2+len(c.Type)+8)*charW + 2*padX
				if c.FKTarget != "" {
					lw += float64(len(c.FKTarget)) * charW
				}
				w = maxFloat(w, lw)
			}
			ih := 0.0
			if len(t.Indexes) > 0 {
				ih = float64(len(t.Indexes))*rowH + 6
			}
			for _, ix := range t.Indexes {
				lw := float64(len(ix.Name)+2+len(ix.Def)+8)*charW + 2*padX
				w = maxFloat(w, lw)
			}
			b := &box{x: colX, y: y, w: w, h: titleH + float64(len(t.Columns))*rowH + ih + padY, fkY: map[string]float64{}}
			for i, c := range t.Columns {
				if c.FKTarget != "" {
					b.fkY[c.Name] = y + titleH + float64(i)*rowH + rowH/2
				}
			}
			boxes[t.Name] = b
			colW = maxFloat(colW, w)
			y += b.h + boxGap
		}
		for _, t := range col {
			boxes[t.Name].w = colW
		}
		colX += colW + colGap
	}

	totalW, totalH := 40.0, 40.0
	for _, b := range boxes {
		totalW = maxFloat(totalW, b.x+b.w+40)
		totalH = maxFloat(totalH, b.y+b.h+40)
	}

	var svg strings.Builder
	esc := html.EscapeString
	// Edges first, under the boxes.
	edges := append([]Edge(nil), s.Edges...)
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].ToTable != edges[j].ToTable {
			return edges[i].ToTable < edges[j].ToTable
		}
		return edges[i].FromTable < edges[j].FromTable
	})
	for _, e := range edges {
		child, parent := boxes[e.FromTable], boxes[e.ToTable]
		if child == nil || parent == nil {
			continue
		}
		y1, ok := child.fkY[e.FromColumn]
		if !ok {
			continue
		}
		x1 := child.x
		x2, y2 := parent.x+parent.w, parent.y+titleH/2
		midX := (x1 + x2) / 2
		fmt.Fprintf(&svg, `<path d="M %.1f %.1f H %.1f V %.1f H %.1f" class="edge" marker-end="url(#arr)"/>`+"\n",
			x1, y1, midX, y2, x2+6)
	}
	for i := range tables {
		t := &tables[i]
		b := boxes[t.Name]
		fmt.Fprintf(&svg, `<g><rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="6" class="tbl"/>`+"\n", b.x, b.y, b.w, b.h)
		fmt.Fprintf(&svg, `<text x="%.1f" y="%.1f" class="title">%s</text>`+"\n", b.x+padX, b.y+20, esc(t.Schema+"."+t.Name))
		fmt.Fprintf(&svg, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" class="rule"/>`+"\n", b.x, b.y+titleH-2, b.x+b.w, b.y+titleH-2)
		for ci, c := range t.Columns {
			y := b.y + titleH + float64(ci)*rowH + 15
			fmt.Fprintf(&svg, `<text x="%.1f" y="%.1f" class="col">%s <tspan class="typ">%s</tspan>`, b.x+padX, y, esc(c.Name), esc(c.Type))
			if c.PK {
				svg.WriteString(` <tspan class="pk">PK</tspan>`)
			}
			if c.FKTarget != "" {
				fmt.Fprintf(&svg, ` <tspan class="fk">FK → %s</tspan>`, esc(c.FKTarget))
			}
			svg.WriteString(`</text>` + "\n")
		}
		if len(t.Indexes) > 0 {
			dy := b.y + titleH + float64(len(t.Columns))*rowH + 3
			fmt.Fprintf(&svg, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" class="rule"/>`+"\n", b.x, dy, b.x+b.w, dy)
			for xi, ix := range t.Indexes {
				y := dy + float64(xi)*rowH + 16
				fmt.Fprintf(&svg, `<text x="%.1f" y="%.1f" class="idx">%s <tspan class="typ">%s</tspan>`, b.x+padX, y, esc(ix.Name), esc(ix.Def))
				if ix.Unique {
					svg.WriteString(` <tspan class="pk">UNIQUE</tspan>`)
				}
				svg.WriteString(`</text>` + "\n")
			}
		}
		svg.WriteString(`</g>` + "\n")
	}

	return `<!doctype html><html><head><meta charset="utf-8">
<title>schema — pgbot erd</title>
<style>
  body{margin:0;background:#0b0d0f;color:#dfe4ea;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;overflow:hidden}
  header{padding:10px 16px;font-size:13px;color:#8b95a3;border-bottom:1px solid #262c33}
  header b{color:#dfe4ea}
  #view{width:100vw;height:calc(100vh - 41px);cursor:grab}
  .tbl{fill:#14171b;stroke:#3a4450;stroke-width:1}
  .title{fill:#5fb0e6;font-weight:600;font-size:14px}
  .rule{stroke:#262c33}
  .col{fill:#dfe4ea;font-size:13px}
  .typ{fill:#8b95a3}
  .pk{fill:#5ecf9a;font-weight:600}
  .fk{fill:#e8a75a}
  .edge{fill:none;stroke:#5a6472;stroke-width:1.5;stroke-dasharray:5 4}
  .idx{fill:#8b95a3;font-size:12px}
</style></head><body>
<header><b>pgbot erd</b> · ` + esc(s.headerLine()) + ` — drag to pan, scroll to zoom · generated locally, nothing leaves this file</header>
<svg id="view" viewBox="0 0 ` + fmt.Sprintf("%.0f %.0f", totalW, totalH) + `">
<defs><marker id="arr" markerWidth="9" markerHeight="8" refX="8" refY="4" orient="auto">
<path d="M9 0 L0 4 L9 8" fill="none" stroke="#5a6472" stroke-width="1.5"/></marker></defs>
<g id="pz" transform="translate(20,20)">
` + svg.String() + `</g></svg>
<script>
(function(){
  var svgEl=document.getElementById('view'),g=document.getElementById('pz');
  var tx=20,ty=20,sc=1,drag=null;
  function apply(){g.setAttribute('transform','translate('+tx+','+ty+') scale('+sc+')');}
  svgEl.addEventListener('pointerdown',function(e){drag={x:e.clientX,y:e.clientY};svgEl.setPointerCapture(e.pointerId);});
  svgEl.addEventListener('pointermove',function(e){if(!drag)return;tx+=e.clientX-drag.x;ty+=e.clientY-drag.y;drag={x:e.clientX,y:e.clientY};apply();});
  svgEl.addEventListener('pointerup',function(){drag=null;});
  svgEl.addEventListener('wheel',function(e){e.preventDefault();var f=e.deltaY<0?1.1:0.9;sc=Math.min(4,Math.max(0.2,sc*f));apply();},{passive:false});
})();
</script></body></html>
`
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
