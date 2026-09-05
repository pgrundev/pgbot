package erd

import (
	"sort"
	"strings"
)

// RenderASCIIRow lays the diagram out left-to-right: referenced (parent)
// tables in the left column, referencing (child) tables one column further
// right, boxes stacked within a column. Edges are drawn DASHED — plain ascii
// dashes, `|` verticals, `+` corners, `<` arrowhead into the parent — so
// relationships read differently from the solid box borders.
func RenderASCIIRow(s Schema) string {
	if len(s.Tables) == 0 {
		return "no tables found (empty schema, or the role cannot see them)\n"
	}

	tables := append([]Table(nil), s.Tables...)
	sort.Slice(tables, func(i, j int) bool { return tables[i].Name < tables[j].Name })
	byName := map[string]*Table{}
	for i := range tables {
		byName[tables[i].Name] = &tables[i]
	}

	// Depth: roots (no FK out, or FK to unknown) at 0; a child sits one right
	// of its deepest parent. Iterate to fixpoint; cycles keep their first depth.
	depth := map[string]int{}
	for _, t := range tables {
		depth[t.Name] = 0
	}
	for iter := 0; iter < len(tables); iter++ {
		changed := false
		for _, e := range s.Edges {
			if _, ok := byName[e.FromTable]; !ok {
				continue
			}
			if d, ok := depth[e.ToTable]; ok && depth[e.FromTable] < d+1 {
				depth[e.FromTable] = d + 1
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

	// Columns: render each box, compute per-column width and stacked heights.
	type placed struct {
		lines         []string
		x0, x1, y0    int // global coordinates; y0 = title row
		fkRowByColumn map[string]int
	}
	cols := make([][]*placed, maxDepth+1)
	pl := map[string]*placed{}
	for i := range tables {
		t := &tables[i]
		var b strings.Builder
		writeTableBox(&b, *t)
		p := &placed{lines: strings.Split(strings.TrimRight(b.String(), "\n"), "\n"),
			fkRowByColumn: map[string]int{}}
		for ci, c := range t.Columns {
			if c.FKTarget != "" {
				p.fkRowByColumn[c.Name] = ci + 1 // relative to box top
			}
		}
		cols[depth[t.Name]] = append(cols[depth[t.Name]], p)
		pl[t.Name] = p
	}

	// Gutter lanes: one vertical track per edge in the gutter left of the
	// child's column, capped so wide fan-ins degrade to the textual marker.
	const lanesPerGutter = 4
	gutterW := make([]int, maxDepth+1) // gutter g sits left of column g (g>=1)
	edgesInGutter := make([]int, maxDepth+2)
	for _, e := range s.Edges {
		if pl[e.FromTable] == nil || pl[e.ToTable] == nil {
			continue
		}
		edgesInGutter[depth[e.FromTable]]++
	}
	for g := 1; g <= maxDepth; g++ {
		gutterW[g] = 4 + 2*minInt(edgesInGutter[g], lanesPerGutter)
	}

	// Place columns: x offsets, and stack boxes vertically with one blank row.
	x := 0
	totalH := 0
	for d := 0; d <= maxDepth; d++ {
		if d > 0 {
			x += gutterW[d]
		}
		w, y := 0, 0
		for _, p := range cols[d] {
			p.x0, p.y0 = x, y
			for _, l := range p.lines {
				w = maxInt(w, len([]rune(l)))
			}
			y += len(p.lines) + 1
		}
		for _, p := range cols[d] {
			p.x1 = x + w - 1
		}
		totalH = maxInt(totalH, y)
		x += w
	}
	totalW := x

	grid := make([][]rune, totalH)
	for i := range grid {
		grid[i] = []rune(strings.Repeat(" ", totalW))
	}
	for _, p := range pl {
		for li, l := range p.lines {
			for ci, r := range []rune(l) {
				grid[p.y0+li][p.x0+ci] = r
			}
		}
	}

	put := func(y, x int, r rune) {
		cur := grid[y][x]
		switch {
		case cur == ' ':
			grid[y][x] = r
		case (cur == '|' && r == '-') || (cur == '-' && r == '|'):
			grid[y][x] = '+'
		}
	}

	// Route: child FK row → dashes left → lane → vertical → parent title row →
	// `<` into the parent's right border. Only adjacent-column edges get a
	// line; longer spans (and over-cap fan-ins) keep their textual FK marker.
	laneUsed := map[int]int{} // gutter → lanes taken
	edges := append([]Edge(nil), s.Edges...)
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].ToTable != edges[j].ToTable {
			return edges[i].ToTable < edges[j].ToTable
		}
		return edges[i].FromTable < edges[j].FromTable
	})
	for _, e := range edges {
		child, parent := pl[e.FromTable], pl[e.ToTable]
		if child == nil || parent == nil {
			continue
		}
		g := depth[e.FromTable]
		if depth[e.ToTable] != g-1 || laneUsed[g] >= lanesPerGutter {
			continue
		}
		lane := laneUsed[g]
		laneUsed[g]++
		fkRel, ok := child.fkRowByColumn[e.FromColumn]
		if !ok {
			continue
		}
		childY := child.y0 + fkRel
		parentY := parent.y0
		laneX := child.x0 - 2 - 2*lane

		for xx := laneX + 1; xx < child.x0; xx++ {
			put(childY, xx, '-')
		}
		if childY != parentY {
			put(childY, laneX, '+')
			put(parentY, laneX, '+')
			lo, hi := minInt(childY, parentY), maxInt(childY, parentY)
			for yy := lo + 1; yy < hi; yy++ {
				put(yy, laneX, '|')
			}
		}
		for xx := parent.x1 + 2; xx < laneX; xx++ {
			put(parentY, xx, '-')
		}
		if childY == parentY {
			put(parentY, laneX, '-')
		}
		grid[parentY][parent.x1+1] = '<'
	}

	var b strings.Builder
	b.WriteString(s.headerLine() + "\n\n")
	for _, row := range grid {
		b.WriteString(strings.TrimRight(string(row), " "))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	writeForest(&b, s)
	return b.String()
}
