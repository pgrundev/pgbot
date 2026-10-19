package erd

import (
	"html"
	"regexp"
	"strings"
	"testing"
)

func TestRenderersDistinguishQualifiedTableIdentity(t *testing.T) {
	s := Schema{Tables: []Table{
		{Schema: "a.b", Name: "users", Columns: []Column{{Name: "id", Type: "bigint", PK: true}}},
		{Schema: "a", Name: "b.users", Columns: []Column{{Name: "id", Type: "bigint", PK: true}}},
		{Schema: "auth", Name: "users", Columns: []Column{{Name: "id", Type: "bigint", PK: true}}},
		{Schema: `销售 "区"`, Name: "订单 表", Columns: []Column{{Name: "id", Type: "bigint", PK: true}}},
	}}

	wantLabels := []string{
		`"a.b".users`,
		`a."b.users"`,
		`auth.users`,
		`"销售 ""区"""."订单 表"`,
	}
	for name, out := range map[string]string{
		"column ASCII": RenderASCII(s, false),
		"row ASCII":    RenderASCIIRow(s),
	} {
		for _, label := range wantLabels {
			if strings.Count(out, label) != 1 {
				t.Errorf("%s must render qualified label %q exactly once:\n%s", name, label, out)
			}
		}
	}

	htmlOut := html.UnescapeString(RenderHTML(s))
	for _, label := range wantLabels {
		if strings.Count(htmlOut, ">"+label+"</text>") != 1 {
			t.Errorf("HTML must render qualified label %q exactly once", label)
		}
	}

	mermaid := RenderMermaid(s)
	aliases := regexp.MustCompile(`(?m)^    (table_[0-9a-f]{64})\["([^"]*)"\] \{$`).FindAllStringSubmatch(mermaid, -1)
	if len(aliases) != len(s.Tables) {
		t.Fatalf("Mermaid must declare each unsafe or colliding table through an opaque ID and alias; got %d declarations:\n%s", len(aliases), mermaid)
	}
	ids := map[string]bool{}
	for _, match := range aliases {
		if ids[match[1]] {
			t.Fatalf("Mermaid reused opaque entity ID %q:\n%s", match[1], mermaid)
		}
		ids[match[1]] = true
	}
	for _, alias := range []string{
		`#quot;a.b#quot;.users`,
		`a.#quot;b.users#quot;`,
		`auth.users`,
		`#quot;销售 #quot;#quot;区#quot;#quot;#quot;.#quot;订单 表#quot;`,
	} {
		if !strings.Contains(mermaid, `[`+`"`+alias+`"`+`]`) {
			t.Errorf("Mermaid missing safe alias %q:\n%s", alias, mermaid)
		}
	}
}

func TestRenderersRouteSchemaQualifiedEdges(t *testing.T) {
	s := Schema{
		Tables: []Table{
			{Schema: "auth", Name: "users", Columns: []Column{{Name: "id", Type: "bigint", PK: true}}},
			{Schema: "crm", Name: "accounts", Columns: []Column{
				{Name: "id", Type: "bigint", PK: true},
				{Name: "user_id", Type: "bigint", FKTarget: "crm.users.id"},
			}},
			{Schema: "crm", Name: "users", Columns: []Column{{Name: "id", Type: "bigint", PK: true}}},
			{Schema: "sales", Name: "orders", Columns: []Column{
				{Name: "id", Type: "bigint", PK: true},
				{Name: "buyer_id", Type: "bigint", FKTarget: "crm.users.id"},
			}},
			{Schema: "sales", Name: "refunds", Columns: []Column{
				{Name: "id", Type: "bigint", PK: true},
				{Name: "order_id", Type: "bigint", FKTarget: "orders.id"},
			}},
		},
		Edges: []Edge{
			{FromSchema: "crm", FromTable: "accounts", FromColumn: "user_id", ToSchema: "crm", ToTable: "users", ToColumn: "id"},
			{FromSchema: "sales", FromTable: "orders", FromColumn: "buyer_id", ToSchema: "crm", ToTable: "users", ToColumn: "id"},
			{FromSchema: "sales", FromTable: "refunds", FromColumn: "order_id", ToSchema: "sales", ToTable: "orders", ToColumn: "id"},
		},
	}

	column := RenderASCII(s, false)
	columnDiagram := strings.Split(column, "\nRelationships\n")[0]
	if got := strings.Count(columnDiagram, "▶"); got != 2 {
		t.Errorf("column ASCII routed %d distinct target arrows, want 2:\n%s", got, column)
	}
	for _, want := range []string{
		"crm.users",
		"├─< accounts (user_id)",
		"└─< orders (buyer_id)",
		"└─< refunds (order_id)",
	} {
		if !strings.Contains(column, want) {
			t.Errorf("column ASCII/forest missing %q:\n%s", want, column)
		}
	}

	row := RenderASCIIRow(s)
	rowDiagram := strings.Split(row, "\nRelationships\n")[0]
	if got := strings.Count(rowDiagram, "<"); got != 2 {
		t.Errorf("row ASCII routed %d distinct target arrows, want 2:\n%s", got, row)
	}

	htmlOut := RenderHTML(s)
	if got := strings.Count(htmlOut, `<path d="M `); got != len(s.Edges) {
		t.Errorf("HTML routed %d paths, want %d", got, len(s.Edges))
	}
	authRect := rectBeforeTableLabel(htmlOut, "auth.users")
	crmRect := rectBeforeTableLabel(htmlOut, "crm.users")
	if authRect == "" || crmRect == "" || authRect == crmRect {
		t.Errorf("same-named HTML tables need distinct boxes; auth=%q crm=%q", authRect, crmRect)
	}

	mermaid := RenderMermaid(s)
	crmID := mermaidEntityID(tableIdentity{Schema: "crm", Name: "users"})
	authID := mermaidEntityID(tableIdentity{Schema: "auth", Name: "users"})
	for _, want := range []string{
		crmID + " ||--o{ accounts : user_id",
		crmID + " ||--o{ orders : buyer_id",
		"orders ||--o{ refunds : order_id",
	} {
		if !strings.Contains(mermaid, want) {
			t.Errorf("Mermaid missing routed relationship %q:\n%s", want, mermaid)
		}
	}
	if strings.Contains(mermaid, authID+" ||--") {
		t.Errorf("Mermaid routed an edge to auth.users instead of crm.users:\n%s", mermaid)
	}
}

func TestLegacyUnqualifiedEdgesRequireUniqueTableNames(t *testing.T) {
	legacy := fixture()
	if out := RenderMermaid(legacy); !strings.Contains(out, "customers ||--o{ orders : customer_id") {
		t.Fatalf("legacy unique-name edges must retain their old Mermaid identity:\n%s", out)
	}

	ambiguous := Schema{
		Tables: []Table{
			{Schema: "auth", Name: "users", Columns: []Column{{Name: "id", Type: "bigint", PK: true}}},
			{Schema: "crm", Name: "users", Columns: []Column{{Name: "id", Type: "bigint", PK: true}}},
			{Schema: "sales", Name: "orders", Columns: []Column{{Name: "buyer_id", Type: "bigint", FKTarget: "users.id"}}},
		},
		Edges: []Edge{{FromTable: "orders", FromColumn: "buyer_id", ToTable: "users", ToColumn: "id"}},
	}
	for name, out := range map[string]string{
		"column ASCII": RenderASCII(ambiguous, false),
		"row ASCII":    RenderASCIIRow(ambiguous),
		"HTML":         RenderHTML(ambiguous),
		"Mermaid":      RenderMermaid(ambiguous),
	} {
		if strings.Contains(out, "▶") || strings.Contains(out, "<--") || strings.Contains(out, `<path d="M `) || strings.Contains(out, " ||--o{ ") {
			t.Errorf("%s guessed a schema for an ambiguous legacy edge:\n%s", name, out)
		}
	}
}

func TestExplicitOutboundEdgeSurvivesFilteredTables(t *testing.T) {
	s := Schema{
		Tables: []Table{{Schema: "sales", Name: "orders", Columns: []Column{
			{Name: "id", Type: "bigint", PK: true},
			{Name: "buyer_id", Type: "bigint", FKTarget: "crm.users.id"},
		}}},
		Edges: []Edge{{
			FromSchema: "sales", FromTable: "orders", FromColumn: "buyer_id",
			ToSchema: "crm", ToTable: "users", ToColumn: "id",
		}},
	}

	for name, out := range map[string]string{
		"column ASCII": RenderASCII(s, false),
		"row ASCII":    RenderASCIIRow(s),
	} {
		if !strings.Contains(out, "FK → crm.users.id") || !strings.Contains(out, "crm.users\n └─< orders (buyer_id)") {
			t.Errorf("%s dropped the explicit outbound target:\n%s", name, out)
		}
	}
	mermaid := RenderMermaid(s)
	targetID := mermaidEntityID(tableIdentity{Schema: "crm", Name: "users"})
	for _, want := range []string{
		targetID + `["crm.users"]`,
		targetID + " ||--o{ orders : buyer_id",
	} {
		if !strings.Contains(mermaid, want) {
			t.Errorf("Mermaid dropped explicit outbound target %q:\n%s", want, mermaid)
		}
	}
	if got := strings.Count(RenderHTML(s), `<path d="M `); got != 0 {
		t.Errorf("HTML drew %d paths without a target box, want 0", got)
	}
}

func rectBeforeTableLabel(rendered, label string) string {
	labelAt := strings.Index(rendered, ">"+html.EscapeString(label)+"</text>")
	if labelAt < 0 {
		return ""
	}
	groupAt := strings.LastIndex(rendered[:labelAt], "<g><rect")
	if groupAt < 0 {
		return ""
	}
	lineEnd := strings.IndexByte(rendered[groupAt:], '\n')
	if lineEnd < 0 {
		return ""
	}
	return rendered[groupAt : groupAt+lineEnd]
}

func TestMermaidAliasesDollarTableNames(t *testing.T) {
	got := RenderMermaid(Schema{Tables: []Table{{Schema: "public", Name: "value$dollar", Columns: []Column{{Name: "id", Type: "bigint"}}}}})
	if strings.Contains(got, "    value$dollar {") || !strings.Contains(got, `["public.value$dollar"]`) {
		t.Fatalf("Mermaid cannot parse a bare dollar sign in an entity ID: %s", got)
	}
}
