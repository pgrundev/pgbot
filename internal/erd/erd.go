// Package erd renders a database's entity-relationship structure — tables,
// columns, keys, foreign-key edges — as a box-drawn terminal diagram or a
// Mermaid erDiagram. Structure only, never data: the same boundary as the
// schema_of MCP tool.
package erd

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pgrundev/pgbot/internal/render"
)

type Column struct {
	Name     string
	Type     string
	PK       bool
	FKTarget string // "table.column" when this column references another table
}

type Table struct {
	Schema  string
	Name    string
	Columns []Column
	Indexes []Index // non-primary indexes (the PK marker already covers its index)
}

// Index is one non-primary index, its definition compacted to method+columns.
type Index struct {
	Name   string
	Def    string // e.g. "btree (customer_id)" — from pg_get_indexdef
	Unique bool
}

// DBInfo is the header line: which database this diagram describes.
type DBInfo struct {
	Database  string
	Version   string
	SizeBytes int64
}

type Edge struct {
	FromTable  string // the referencing (child) table
	FromColumn string
	ToTable    string // the referenced (parent) table
	ToColumn   string
}

type Schema struct {
	Tables []Table
	Edges  []Edge
	Info   DBInfo
}

// headerLine summarizes the database and the diagram: name, server version,
// size, and the counts of what is drawn.
func (s Schema) headerLine() string {
	idx := 0
	for _, t := range s.Tables {
		idx += len(t.Indexes)
	}
	parts := []string{}
	if s.Info.Database != "" {
		parts = append(parts, s.Info.Database)
	}
	if s.Info.Version != "" {
		parts = append(parts, s.Info.Version)
	}
	parts = append(parts, fmt.Sprintf("%d tables", len(s.Tables)), fmt.Sprintf("%d FKs", len(s.Edges)))
	if idx > 0 {
		parts = append(parts, fmt.Sprintf("%d indexes", idx))
	}
	if s.Info.SizeBytes > 0 {
		parts = append(parts, render.HumanBytes(s.Info.SizeBytes))
	}
	return strings.Join(parts, " · ")
}

// RenderASCII draws one box per table (name, columns, PK/FK markers) with each
// foreign key ROUTED as a line in a left gutter — corner at the FK row, a
// vertical lane, an arrowhead into the parent's title row — followed by a
// crow's-foot relationship forest. Deterministic: sorted tables, sorted edges,
// lanes assigned in order.
func RenderASCII(s Schema, color bool) string {
	if len(s.Tables) == 0 {
		return "no tables found (empty schema, or the role cannot see them)\n"
	}
	var b strings.Builder
	b.WriteString(s.headerLine() + "\n\n")

	tables := append([]Table(nil), s.Tables...)
	sort.Slice(tables, func(i, j int) bool {
		if tables[i].Schema != tables[j].Schema {
			return tables[i].Schema < tables[j].Schema
		}
		return tables[i].Name < tables[j].Name
	})

	// Render boxes to lines, remembering each table's title row and each FK
	// column's row.
	var lines []string
	titleRow := map[string]int{}
	type conn struct{ childRow, parentRow int }
	var conns []conn
	var fkRows []struct {
		row    int
		target string // parent table name
	}
	for _, t := range tables {
		titleRow[t.Name] = len(lines)
		var box strings.Builder
		writeTableBox(&box, t)
		boxLines := strings.Split(strings.TrimRight(box.String(), "\n"), "\n")
		for i, c := range t.Columns {
			if c.FKTarget != "" {
				parent := c.FKTarget
				if dot := strings.IndexByte(parent, '.'); dot > 0 {
					parent = parent[:dot]
				}
				fkRows = append(fkRows, struct {
					row    int
					target string
				}{len(lines) + 1 + i, parent})
			}
		}
		lines = append(lines, boxLines...)
		lines = append(lines, "")
	}
	for _, fk := range fkRows {
		if pr, ok := titleRow[fk.target]; ok {
			conns = append(conns, conn{childRow: fk.row, parentRow: pr})
		}
	}

	// Lane assignment: longest spans take the outer lanes; a lane is reused
	// when row ranges don't overlap. Capped so a monster schema degrades to
	// the textual FK markers instead of an unreadable gutter.
	const maxLanes = 8
	type lane struct{ spans [][2]int }
	var lanes []lane
	laneOf := make([]int, len(conns))
	sort.SliceStable(conns, func(i, j int) bool {
		si := abs(conns[i].childRow - conns[i].parentRow)
		sj := abs(conns[j].childRow - conns[j].parentRow)
		if si != sj {
			return si > sj
		}
		return conns[i].childRow < conns[j].childRow
	})
	for i, c := range conns {
		lo, hi := minInt(c.childRow, c.parentRow), maxInt(c.childRow, c.parentRow)
		laneOf[i] = -1
		for li := range lanes {
			free := true
			for _, sp := range lanes[li].spans {
				if lo <= sp[1] && sp[0] <= hi {
					free = false
					break
				}
			}
			if free {
				lanes[li].spans = append(lanes[li].spans, [2]int{lo, hi})
				laneOf[i] = li
				break
			}
		}
		if laneOf[i] == -1 && len(lanes) < maxLanes {
			lanes = append(lanes, lane{spans: [][2]int{{lo, hi}}})
			laneOf[i] = len(lanes) - 1
		}
	}

	gw := len(lanes) * 2 // gutter width: 2 columns per lane
	if gw > 0 {
		gw += 2 // room for the horizontal run and arrowhead next to the boxes
		grid := make([][]rune, len(lines))
		for i := range grid {
			grid[i] = []rune(strings.Repeat(" ", gw))
		}
		put := func(row, col int, r rune) {
			cur := grid[row][col]
			switch {
			case cur == ' ':
				grid[row][col] = r
			case (cur == '│' && r == '─') || (cur == '─' && r == '│'):
				grid[row][col] = '┼'
			}
		}
		for i, c := range conns {
			if laneOf[i] < 0 {
				continue // over the lane cap: the FK → marker still tells the story
			}
			col := laneOf[i] * 2 // outer lanes (longest spans) leftmost
			lo, hi := minInt(c.childRow, c.parentRow), maxInt(c.childRow, c.parentRow)
			for r := lo + 1; r < hi; r++ {
				put(r, col, '│')
			}
			topCorner, botCorner := '┌', '└'
			for x := col + 1; x < gw-1; x++ {
				put(lo, x, '─')
				put(hi, x, '─')
			}
			put(lo, col, topCorner)
			put(hi, col, botCorner)
			// Arrowhead into the parent row, plain run into the child row.
			if c.parentRow < c.childRow {
				grid[c.parentRow][gw-1] = '▶'
				put(c.childRow, gw-1, '─')
			} else {
				grid[c.parentRow][gw-1] = '▶'
				put(c.childRow, gw-1, '─')
			}
		}
		for i, l := range lines {
			b.WriteString(strings.TrimRight(string(grid[i])+l, " "))
			b.WriteString("\n")
		}
	} else {
		for _, l := range lines {
			b.WriteString(l + "\n")
		}
	}
	writeForest(&b, s)
	return b.String()
}

func abs(a int) int {
	if a < 0 {
		return -a
	}
	return a
}
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// writeTableBox renders one table:
//
//	┌─ public.orders ───────────────────┐
//	│ id           bigint   PK          │
//	│ customer_id  bigint   FK → customers.id │
//	└───────────────────────────────────┘
func writeTableBox(b *strings.Builder, t Table) {
	nameW, typeW := 0, 0
	for _, c := range t.Columns {
		nameW = max(nameW, len(c.Name))
		typeW = max(typeW, len(c.Type))
	}
	var rows []string
	for _, c := range t.Columns {
		marker := ""
		switch {
		case c.PK && c.FKTarget != "":
			marker = "PK FK → " + c.FKTarget
		case c.PK:
			marker = "PK"
		case c.FKTarget != "":
			marker = "FK → " + c.FKTarget
		}
		rows = append(rows, strings.TrimRight(
			fmt.Sprintf("%-*s  %-*s  %s", nameW, c.Name, typeW, c.Type, marker), " "))
	}
	var idxRows []string
	for _, ix := range t.Indexes {
		row := ix.Name + "  " + ix.Def
		if ix.Unique {
			row += "  UNIQUE"
		}
		idxRows = append(idxRows, row)
	}
	title := t.Schema + "." + t.Name
	inner := len(title) + 4
	for _, r := range append(append([]string(nil), rows...), idxRows...) {
		inner = max(inner, len(r)+2)
	}
	fmt.Fprintf(b, "┌─ %s %s┐\n", title, strings.Repeat("─", inner-len(title)-3))
	for _, r := range rows {
		fmt.Fprintf(b, "│ %-*s│\n", inner-1, r)
	}
	if len(idxRows) > 0 {
		fmt.Fprintf(b, "├%s┤\n", strings.Repeat("─", inner))
		for _, r := range idxRows {
			fmt.Fprintf(b, "│ %-*s│\n", inner-1, r)
		}
	}
	fmt.Fprintf(b, "└%s┘\n", strings.Repeat("─", inner))
}

// writeForest prints the FK graph as parent-owns-children trees:
//
//	customers
//	 └─< orders (customer_id)
//	     └─< order_items (order_id)   · also < products
//
// Each child appears once, under its first (alphabetical) parent; additional
// parents show as a cross-link. Cycle-safe via a visited set.
func writeForest(b *strings.Builder, s Schema) {
	if len(s.Edges) == 0 {
		return
	}
	b.WriteString("Relationships\n")

	children := map[string][]Edge{} // parent → edges into it
	firstParent := map[string]string{}
	hasParent := map[string]bool{}
	edges := append([]Edge(nil), s.Edges...)
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].ToTable != edges[j].ToTable {
			return edges[i].ToTable < edges[j].ToTable
		}
		return edges[i].FromTable < edges[j].FromTable
	})
	for _, e := range edges {
		children[e.ToTable] = append(children[e.ToTable], e)
		hasParent[e.FromTable] = true
		if _, ok := firstParent[e.FromTable]; !ok {
			firstParent[e.FromTable] = e.ToTable
		}
	}

	var roots []string
	for parent := range children {
		if !hasParent[parent] {
			roots = append(roots, parent)
		}
	}
	sort.Strings(roots)

	visited := map[string]bool{}
	var walk func(table, indent string)
	walk = func(table, indent string) {
		if visited[table] {
			return
		}
		visited[table] = true
		kids := children[table]
		for i, e := range kids {
			branch := "├─<"
			childIndent := indent + "│   "
			if i == len(kids)-1 {
				branch = "└─<"
				childIndent = indent + "    "
			}
			line := fmt.Sprintf("%s%s %s (%s)", indent, branch, e.FromTable, e.FromColumn)
			if firstParent[e.FromTable] != table {
				line += "  · also above"
				fmt.Fprintln(b, line)
				continue
			}
			fmt.Fprintln(b, line)
			walk(e.FromTable, childIndent)
		}
	}
	for _, r := range roots {
		fmt.Fprintln(b, r)
		walk(r, " ")
	}
	// Cycles (every member has a parent) still deserve printing.
	var leftovers []string
	for parent := range children {
		if !visited[parent] {
			leftovers = append(leftovers, parent)
		}
	}
	sort.Strings(leftovers)
	for _, r := range leftovers {
		fmt.Fprintln(b, r+"  (cycle)")
		walk(r, " ")
	}
}

// RenderMermaid emits a mermaid erDiagram — pasteable into GitHub markdown or
// mermaid.live for an interactive pan/zoom view.
func RenderMermaid(s Schema) string {
	var b strings.Builder
	b.WriteString("erDiagram\n")
	edges := append([]Edge(nil), s.Edges...)
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].ToTable != edges[j].ToTable {
			return edges[i].ToTable < edges[j].ToTable
		}
		return edges[i].FromTable < edges[j].FromTable
	})
	for _, e := range edges {
		fmt.Fprintf(&b, "    %s ||--o{ %s : %s\n", e.ToTable, e.FromTable, e.FromColumn)
	}
	tables := append([]Table(nil), s.Tables...)
	sort.Slice(tables, func(i, j int) bool { return tables[i].Name < tables[j].Name })
	for _, t := range tables {
		fmt.Fprintf(&b, "    %s {\n", t.Name)
		for _, c := range t.Columns {
			marker := ""
			switch {
			case c.PK && c.FKTarget != "":
				marker = " PK, FK"
			case c.PK:
				marker = " PK"
			case c.FKTarget != "":
				marker = " FK"
			}
			// Mermaid types must be bare words: "character varying(64)" breaks it.
			typ := strings.NewReplacer(" ", "_", "(", "_", ")", "", ",", "_").Replace(c.Type)
			fmt.Fprintf(&b, "        %s %s%s\n", typ, c.Name, marker)
		}
		b.WriteString("    }\n")
	}
	return b.String()
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
