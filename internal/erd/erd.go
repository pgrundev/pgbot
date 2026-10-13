// Package erd renders a database's entity-relationship structure — tables,
// columns, keys, foreign-key edges — as a box-drawn terminal diagram or a
// Mermaid erDiagram. Structure only, never data: the same boundary as the
// schema_of MCP tool.
package erd

import (
	"crypto/sha256"
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
	FromSchema string
	FromTable  string // the referencing (child) table
	FromColumn string
	ToSchema   string
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
	titleRow := map[tableIdentity]int{}
	columnRow := map[columnIdentity]int{}
	type conn struct{ childRow, parentRow int }
	var conns []conn
	for _, t := range tables {
		id := identityOf(t)
		titleRow[id] = len(lines)
		var box strings.Builder
		writeTableBox(&box, t)
		boxLines := strings.Split(strings.TrimRight(box.String(), "\n"), "\n")
		for i, c := range t.Columns {
			columnRow[columnIdentity{Table: id, Column: c.Name}] = len(lines) + 1 + i
		}
		lines = append(lines, boxLines...)
		lines = append(lines, "")
	}
	for _, e := range resolveEdges(tables, s.Edges) {
		child, childOK := columnRow[columnIdentity{Table: e.From, Column: e.Edge.FromColumn}]
		parent, parentOK := titleRow[e.To]
		if childOK && parentOK {
			conns = append(conns, conn{childRow: child, parentRow: parent})
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

type tableIdentity struct {
	Schema string
	Name   string
}

type columnIdentity struct {
	Table  tableIdentity
	Column string
}

type resolvedEdge struct {
	Edge        Edge
	From        tableIdentity
	To          tableIdentity
	FromPresent bool
	ToPresent   bool
}

func identityOf(t Table) tableIdentity {
	return tableIdentity{Schema: t.Schema, Name: t.Name}
}

func lessIdentity(a, b tableIdentity) bool {
	if a.Schema != b.Schema {
		return a.Schema < b.Schema
	}
	return a.Name < b.Name
}

func resolveTableIdentity(tables []Table, schema, name string) (tableIdentity, bool, bool) {
	var match tableIdentity
	matches := 0
	for _, table := range tables {
		if table.Name != name || (schema != "" && table.Schema != schema) {
			continue
		}
		match = identityOf(table)
		matches++
	}
	if matches == 1 {
		return match, true, true
	}
	if matches > 1 {
		return tableIdentity{}, false, false
	}
	return tableIdentity{Schema: schema, Name: name}, false, true
}

func resolveEdges(tables []Table, edges []Edge) []resolvedEdge {
	resolved := make([]resolvedEdge, 0, len(edges))
	for _, edge := range edges {
		from, fromPresent, fromOK := resolveTableIdentity(tables, edge.FromSchema, edge.FromTable)
		to, toPresent, toOK := resolveTableIdentity(tables, edge.ToSchema, edge.ToTable)
		if fromOK && toOK {
			resolved = append(resolved, resolvedEdge{
				Edge: edge, From: from, To: to,
				FromPresent: fromPresent, ToPresent: toPresent,
			})
		}
	}
	sort.Slice(resolved, func(i, j int) bool {
		if resolved[i].To != resolved[j].To {
			return lessIdentity(resolved[i].To, resolved[j].To)
		}
		if resolved[i].From != resolved[j].From {
			return lessIdentity(resolved[i].From, resolved[j].From)
		}
		if resolved[i].Edge.FromColumn != resolved[j].Edge.FromColumn {
			return resolved[i].Edge.FromColumn < resolved[j].Edge.FromColumn
		}
		return resolved[i].Edge.ToColumn < resolved[j].Edge.ToColumn
	})
	return resolved
}

func drawableEdges(edges []resolvedEdge) []resolvedEdge {
	drawable := make([]resolvedEdge, 0, len(edges))
	for _, edge := range edges {
		if edge.FromPresent && edge.ToPresent {
			drawable = append(drawable, edge)
		}
	}
	return drawable
}

func simpleIdentifier(s string) bool {
	if s == "" || !((s[0] >= 'a' && s[0] <= 'z') || s[0] == '_') {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '$') {
			return false
		}
	}
	return true
}

func displayIdentifier(s string) string {
	if simpleIdentifier(s) {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func qualifiedTableName(id tableIdentity) string {
	if id.Schema == "" {
		return displayIdentifier(id.Name)
	}
	return displayIdentifier(id.Schema) + "." + displayIdentifier(id.Name)
}

func duplicateIdentityNames(tables []Table, edges []resolvedEdge) map[string]bool {
	identities := map[tableIdentity]bool{}
	for _, table := range tables {
		identities[identityOf(table)] = true
	}
	for _, edge := range edges {
		identities[edge.From] = true
		identities[edge.To] = true
	}
	counts := map[string]int{}
	for id := range identities {
		counts[id.Name]++
	}
	duplicates := map[string]bool{}
	for name, count := range counts {
		duplicates[name] = count > 1
	}
	return duplicates
}

func forestTableName(id tableIdentity, duplicates map[string]bool, present bool) string {
	if duplicates[id.Name] || (!present && id.Schema != "") {
		return qualifiedTableName(id)
	}
	return id.Name
}

func mermaidEntityID(id tableIdentity) string {
	payload := fmt.Sprintf("%d:%s%d:%s", len(id.Schema), id.Schema, len(id.Name), id.Name)
	sum := sha256.Sum256([]byte(payload))
	return fmt.Sprintf("table_%x", sum)
}

func mermaidSafeEntityName(name string) bool {
	if !simpleIdentifier(name) || strings.Contains(name, "$") {
		return false
	}
	switch strings.ToLower(name) {
	case "class", "classdef", "direction", "end", "erdiagram", "many", "one", "only", "style", "subgraph", "to", "u", "zero":
		return false
	}
	return true
}

func mermaidText(s string) string {
	return strings.NewReplacer(
		"#", "#35;",
		`"`, "#quot;",
		"\r", "#13;",
		"\n", "#10;",
	).Replace(s)
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
	title := qualifiedTableName(identityOf(t))
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
	edges := resolveEdges(s.Tables, s.Edges)
	if len(edges) == 0 {
		return
	}
	b.WriteString("Relationships\n")

	children := map[tableIdentity][]resolvedEdge{} // parent → edges into it
	firstParent := map[tableIdentity]tableIdentity{}
	hasParent := map[tableIdentity]bool{}
	for _, e := range edges {
		children[e.To] = append(children[e.To], e)
		hasParent[e.From] = true
		if _, ok := firstParent[e.From]; !ok {
			firstParent[e.From] = e.To
		}
	}
	duplicates := duplicateIdentityNames(s.Tables, edges)
	present := map[tableIdentity]bool{}
	for _, table := range s.Tables {
		present[identityOf(table)] = true
	}

	var roots []tableIdentity
	for parent := range children {
		if !hasParent[parent] {
			roots = append(roots, parent)
		}
	}
	sort.Slice(roots, func(i, j int) bool { return lessIdentity(roots[i], roots[j]) })

	visited := map[tableIdentity]bool{}
	var walk func(table tableIdentity, indent string)
	walk = func(table tableIdentity, indent string) {
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
			line := fmt.Sprintf("%s%s %s (%s)", indent, branch, forestTableName(e.From, duplicates, present[e.From]), e.Edge.FromColumn)
			if firstParent[e.From] != table {
				line += "  · also above"
				fmt.Fprintln(b, line)
				continue
			}
			fmt.Fprintln(b, line)
			walk(e.From, childIndent)
		}
	}
	for _, r := range roots {
		fmt.Fprintln(b, forestTableName(r, duplicates, present[r]))
		walk(r, " ")
	}
	// Cycles (every member has a parent) still deserve printing.
	var leftovers []tableIdentity
	for parent := range children {
		if !visited[parent] {
			leftovers = append(leftovers, parent)
		}
	}
	sort.Slice(leftovers, func(i, j int) bool { return lessIdentity(leftovers[i], leftovers[j]) })
	for _, r := range leftovers {
		fmt.Fprintln(b, forestTableName(r, duplicates, present[r])+"  (cycle)")
		walk(r, " ")
	}
}

// RenderMermaid emits a mermaid erDiagram — pasteable into GitHub markdown or
// mermaid.live for an interactive pan/zoom view.
func RenderMermaid(s Schema) string {
	var b strings.Builder
	b.WriteString("erDiagram\n")
	tables := append([]Table(nil), s.Tables...)
	sort.Slice(tables, func(i, j int) bool { return lessIdentity(identityOf(tables[i]), identityOf(tables[j])) })
	edges := resolveEdges(tables, s.Edges)
	duplicates := duplicateIdentityNames(tables, edges)
	present := map[tableIdentity]bool{}
	identities := map[tableIdentity]bool{}
	forceAlias := map[tableIdentity]bool{}
	for _, table := range tables {
		id := identityOf(table)
		present[id] = true
		identities[id] = true
	}
	for _, edge := range edges {
		identities[edge.From] = true
		identities[edge.To] = true
		forceAlias[edge.From] = forceAlias[edge.From] || (!edge.FromPresent && edge.From.Schema != "")
		forceAlias[edge.To] = forceAlias[edge.To] || (!edge.ToPresent && edge.To.Schema != "")
	}
	orderedIdentities := make([]tableIdentity, 0, len(identities))
	for id := range identities {
		orderedIdentities = append(orderedIdentities, id)
	}
	sort.Slice(orderedIdentities, func(i, j int) bool { return lessIdentity(orderedIdentities[i], orderedIdentities[j]) })

	tableNames := map[tableIdentity]string{}
	declarations := map[tableIdentity]string{}
	usedNames := map[string]bool{}
	for _, id := range orderedIdentities {
		aliased := duplicates[id.Name] || !mermaidSafeEntityName(id.Name) || forceAlias[id]
		name := id.Name
		if aliased {
			name = mermaidEntityID(id)
		}
		if usedNames[name] {
			aliased = true
			base := mermaidEntityID(id)
			name = base
			for suffix := 2; usedNames[name]; suffix++ {
				name = fmt.Sprintf("%s_%d", base, suffix)
			}
		}
		usedNames[name] = true
		tableNames[id] = name
		declarations[id] = name
		if aliased {
			declarations[id] += `["` + mermaidText(qualifiedTableName(id)) + `"]`
		}
	}
	for _, t := range tables {
		id := identityOf(t)
		fmt.Fprintf(&b, "    %s {\n", declarations[id])
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
	for _, id := range orderedIdentities {
		if !present[id] && declarations[id] != tableNames[id] {
			fmt.Fprintf(&b, "    %s\n", declarations[id])
		}
	}
	for _, e := range edges {
		fmt.Fprintf(&b, "    %s ||--o{ %s : %s\n", tableNames[e.To], tableNames[e.From], e.Edge.FromColumn)
	}
	return b.String()
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
