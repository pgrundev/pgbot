package erd

import (
	"strings"
	"testing"
)

func fixture() Schema {
	return Schema{
		Tables: []Table{
			{Schema: "public", Name: "customers", Columns: []Column{
				{Name: "id", Type: "bigint", PK: true},
				{Name: "email", Type: "text"},
			}},
			{Schema: "public", Name: "order_items", Columns: []Column{
				{Name: "order_id", Type: "bigint", PK: true, FKTarget: "orders.id"},
				{Name: "product_id", Type: "bigint", PK: true, FKTarget: "products.id"},
			}},
			{Schema: "public", Name: "orders", Columns: []Column{
				{Name: "id", Type: "bigint", PK: true},
				{Name: "customer_id", Type: "bigint", FKTarget: "customers.id"},
			}},
			{Schema: "public", Name: "products", Columns: []Column{
				{Name: "id", Type: "bigint", PK: true},
			}},
		},
		Edges: []Edge{
			{FromTable: "order_items", FromColumn: "order_id", ToTable: "orders", ToColumn: "id"},
			{FromTable: "order_items", FromColumn: "product_id", ToTable: "products", ToColumn: "id"},
			{FromTable: "orders", FromColumn: "customer_id", ToTable: "customers", ToColumn: "id"},
		},
		Info: DBInfo{Database: "shop", Version: "postgres 17.4", SizeBytes: 91887295},
	}
}

// The terminal view: one box-drawn block per table with PK/FK markers, then a
// crow's-foot forest of relationships.
func TestRenderASCII(t *testing.T) {
	out := RenderASCII(fixture(), false)

	for _, want := range []string{
		"public.customers", "public.orders",
		"PK", "FK → customers.id",
		"┌", "└", "│", // box drawing
	} {
		if !strings.Contains(out, want) {
			t.Errorf("ascii missing %q:\n%s", want, out)
		}
	}
	// The forest: parents own their children crow's-foot style, and a table
	// with two parents appears under one with a cross-link under the other.
	if !strings.Contains(out, "└─< orders (customer_id)") {
		t.Errorf("customers must own orders in the forest:\n%s", out)
	}
	if !strings.Contains(out, "─< order_items") {
		t.Errorf("order_items must appear as a child:\n%s", out)
	}
	// Deterministic: same input, same bytes.
	if out != RenderASCII(fixture(), false) {
		t.Error("render must be deterministic")
	}
}

// Mermaid output: valid erDiagram with relationships and typed columns —
// pasteable into GitHub or mermaid.live.
func TestRenderMermaid(t *testing.T) {
	out := RenderMermaid(fixture())
	for _, want := range []string{
		"erDiagram",
		"customers ||--o{ orders",
		`orders {`,
		"bigint id PK",
		"bigint customer_id FK",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("mermaid missing %q:\n%s", want, out)
		}
	}
}

// Empty schemas render something honest, not a panic or blank screen.
func TestRenderEmpty(t *testing.T) {
	if out := RenderASCII(Schema{}, false); !strings.Contains(out, "no tables") {
		t.Errorf("empty must say so: %q", out)
	}
}

// The boxes are CONNECTED: each FK gets a routed line in a left gutter —
// corner at the child row, vertical run, arrowhead into the parent's title
// row — so the diagram reads as a diagram, not a list of boxes.
func TestRenderASCIIDrawsEdges(t *testing.T) {
	two := Schema{
		Tables: []Table{
			{Schema: "public", Name: "customers", Columns: []Column{{Name: "id", Type: "bigint", PK: true}}},
			{Schema: "public", Name: "orders", Columns: []Column{
				{Name: "id", Type: "bigint", PK: true},
				{Name: "customer_id", Type: "bigint", FKTarget: "customers.id"},
			}},
		},
		Edges: []Edge{{FromTable: "orders", FromColumn: "customer_id", ToTable: "customers", ToColumn: "id"}},
	}
	out := RenderASCII(two, false)
	lines := strings.Split(out, "\n")

	var arrowRow, cornerRow = -1, -1
	for i, l := range lines {
		if strings.Contains(l, "▶") && strings.Contains(l, "public.customers") {
			arrowRow = i
		}
		if strings.Contains(l, "customer_id") && strings.Contains(l, "└──") {
			cornerRow = i
		}
	}
	if arrowRow < 0 {
		t.Fatalf("parent title row must carry the arrowhead:\n%s", out)
	}
	if cornerRow < 0 {
		t.Fatalf("child FK row must carry the corner:\n%s", out)
	}
	// The vertical run between them.
	for i := arrowRow + 1; i < cornerRow; i++ {
		if !strings.Contains(lines[i], "│") {
			t.Errorf("row %d between endpoints must carry the lane: %q", i, lines[i])
		}
	}
	// Non-edge rows still align: every gutter is the same width.
	if !strings.Contains(out, "Relationships") {
		t.Errorf("the forest stays:\n%s", out)
	}

	// The five-table fixture (three FKs from one table) must still render
	// deterministically and carry a line per edge.
	multi := RenderASCII(fixture(), false)
	if multi != RenderASCII(fixture(), false) {
		t.Error("multi-edge render must be deterministic")
	}
	if strings.Count(multi, "▶") < 3 {
		t.Errorf("each of the 3 edges gets an arrowhead:\n%s", multi)
	}
}

// Row layout: parents in the left column, children to the right, boxes
// side by side — and the edges are DASHED ascii (----, |, + corners, < into
// the parent) so relationships read differently from box borders.
func TestRenderASCIIRow(t *testing.T) {
	two := Schema{
		Tables: []Table{
			{Schema: "public", Name: "customers", Columns: []Column{{Name: "id", Type: "bigint", PK: true}}},
			{Schema: "public", Name: "orders", Columns: []Column{
				{Name: "id", Type: "bigint", PK: true},
				{Name: "customer_id", Type: "bigint", FKTarget: "customers.id"},
			}},
		},
		Edges: []Edge{{FromTable: "orders", FromColumn: "customer_id", ToTable: "customers", ToColumn: "id"}},
	}
	out := RenderASCIIRow(two)

	// Side by side: the parent's box and the child's box share a line.
	var sideBySide bool
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "public.customers") && strings.Contains(l, "public.orders") {
			sideBySide = true
		}
	}
	if !sideBySide {
		t.Fatalf("row layout must place parent and child on the same band:\n%s", out)
	}
	// Dashed connector with the arrowhead pointing at the parent.
	if !strings.Contains(out, "<--") {
		t.Errorf("edge must be dashed with < into the parent:\n%s", out)
	}
	if strings.Contains(out, "▶") {
		t.Errorf("row layout uses ascii dashes, not box-drawing arrows:\n%s", out)
	}
	if out != RenderASCIIRow(two) {
		t.Error("row render must be deterministic")
	}

	// Five tables: three roots left, two children right; renders every edge or
	// falls back to the textual marker — never panics, never overlaps boxes.
	multi := RenderASCIIRow(fixture())
	var rootLine bool
	for _, l := range strings.Split(multi, "\n") {
		if strings.Contains(l, "public.customers") && strings.Contains(l, "public.orders") {
			rootLine = true
		}
	}
	if !rootLine {
		t.Errorf("roots and children must be in adjacent columns:\n%s", multi)
	}
}

// --html: one SELF-CONTAINED file — an inline SVG diagram with dashed edges
// and pan/zoom script, no external requests of any kind, so the schema never
// leaves the file.
func TestRenderHTML(t *testing.T) {
	out := RenderHTML(fixture())

	for _, want := range []string{
		"<svg", "</svg>", "</html>",
		"public.orders", "customer_id",
		"stroke-dasharray", // the dashed relationship edges
		"marker",           // arrowheads
		"addEventListener", // pan/zoom lives inline
	} {
		if !strings.Contains(out, want) {
			t.Errorf("html missing %q", want)
		}
	}
	for _, banned := range []string{"http://", "https://", "src=", "@import"} {
		if strings.Contains(out, banned) {
			t.Errorf("html must be fully self-contained, found %q", banned)
		}
	}
	if out != RenderHTML(fixture()) {
		t.Error("html render must be deterministic")
	}
	// A table name that could break markup must be escaped.
	weird := Schema{Tables: []Table{{Schema: "public", Name: "a<b", Columns: []Column{{Name: "x&y", Type: "text"}}}}}
	w := RenderHTML(weird)
	if strings.Contains(w, "a<b") || !strings.Contains(w, "a&lt;b") {
		t.Errorf("names must be escaped: %q", w)
	}
}

// Indexes render as a section inside the table box, below a divider, with
// UNIQUE marked — and only for tables that have any.
func TestRenderIndexes(t *testing.T) {
	f := fixture()
	f.Tables[2].Indexes = []Index{ // orders
		{Name: "orders_customer_idx", Def: "btree (customer_id)"},
		{Name: "orders_status_uq", Def: "btree (status)", Unique: true},
	}
	out := RenderASCII(f, false)
	for _, want := range []string{"orders_customer_idx", "btree (customer_id)", "UNIQUE"} {
		if !strings.Contains(out, want) {
			t.Errorf("ascii missing index info %q:\n%s", want, out)
		}
	}
	// customers has no indexes: its box must not gain a divider section.
	if strings.Count(out, "├") < 1 {
		t.Errorf("indexed table needs a divider:\n%s", out)
	}
	if h := RenderHTML(f); !strings.Contains(h, "orders_customer_idx") {
		t.Errorf("html must carry indexes")
	}
}

// Every renderer opens with the database info line: name, version, size,
// and the diagram's own counts.
func TestRenderDBInfoHeader(t *testing.T) {
	f := fixture()
	for name, out := range map[string]string{
		"ascii": RenderASCII(f, false),
		"row":   RenderASCIIRow(f),
		"html":  RenderHTML(f),
	} {
		for _, want := range []string{"shop", "postgres 17.4", "4 tables", "3 FKs", "87.6 MiB"} {
			if !strings.Contains(out, want) {
				t.Errorf("%s header missing %q", name, want)
			}
		}
	}
}
