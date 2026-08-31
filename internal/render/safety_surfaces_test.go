package render

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// coveredSurfaces are the output/export renderers exercised by
// TestSafety_everySurfaceCarriesGuards below. TestSafety_noUndiscoveredSurface
// fails the build if an exported (io.Writer, …model.Context…) renderer exists that
// is NOT in this set — so a new surface can't ship silently without carrying the
// destructive-action guards.
var coveredSurfaces = map[string]bool{
	"JSON":            true, // structured: full safety object
	"SARIF":           true, // text message
	"JUnit":           true, // <failure> text
	"Terminal":        true, // grouped + --full
	"Prometheus":      true, // destructive="true" label
	"PrometheusAll":   true, // multi-db variant of Prometheus (same emission)
	"CockroachScreen": true, // focused terminal screen; index findings retain DROP guards
}

// TestSafety_everySurfaceCarriesGuards renders a guarded context through every
// output surface and asserts the destructive-action guard is present — as
// structured data, text, or a label, whichever the format supports.
func TestSafety_everySurfaceCarriesGuards(t *testing.T) {
	render := func(fn func(*bytes.Buffer) error) string {
		var b bytes.Buffer
		if err := fn(&b); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	cases := []struct {
		surface string
		out     string
		want    []string
	}{
		{"JSON", render(func(b *bytes.Buffer) error { return JSON(b, guardedContext()) }),
			[]string{"blocking_caveats", "unused_index.per_node", "DROP INDEX"}},
		{"SARIF", render(func(b *bytes.Buffer) error { return SARIF(b, guardedContext()) }),
			[]string{"DROP INDEX", "the index is unused on every replica"}},
		{"JUnit", render(func(b *bytes.Buffer) error { return JUnit(b, guardedContext(), "warn") }),
			[]string{"DROP INDEX", "the index is unused on every replica", "VACUUM FULL"}},
		{"Terminal-grouped", render(func(b *bytes.Buffer) error { return Terminal(b, guardedContext(), Options{Width: 100}) }),
			[]string{"DROP INDEX", "VACUUM FULL"}},
		{"Terminal-full", render(func(b *bytes.Buffer) error { return Terminal(b, guardedContext(), Options{Full: true, Width: 100}) }),
			[]string{"DROP INDEX", "VACUUM FULL"}},
		{"CockroachScreen", render(func(b *bytes.Buffer) error {
			c := guardedContext()
			c.Server.Engine = "cockroachdb"
			return CockroachScreen(b, c, "indexes", Options{Width: 100})
		}), []string{"DROP INDEX", "the index is unused on every replica"}},
		{"Prometheus", render(func(b *bytes.Buffer) error { return Prometheus(b, guardedContext()) }),
			[]string{`destructive="true"`}},
	}
	for _, tc := range cases {
		for _, w := range tc.want {
			if !strings.Contains(tc.out, w) {
				t.Errorf("%s surface dropped the guard: missing %q", tc.surface, w)
			}
		}
	}
}

// TestSafety_noUndiscoveredSurface is the auto-discovery backstop: it scans the
// render package for exported renderers — an exported package-level func whose
// first parameter is io.Writer and which takes a model.Context — and fails if any
// is not in coveredSurfaces. Add a new output format, and this fails until it is
// exercised by the guard-coverage test above.
func TestSafety_noUndiscoveredSurface(t *testing.T) {
	// Parse each non-test .go file in the render package (ParseFile, not the
	// deprecated ParseDir).
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	discovered := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !fn.Name.IsExported() {
				continue
			}
			if fn.Type.Params == nil || len(fn.Type.Params.List) == 0 {
				continue
			}
			if typeString(fn.Type.Params.List[0].Type) != "io.Writer" {
				continue
			}
			refsContext := false
			for _, p := range fn.Type.Params.List {
				if strings.Contains(typeString(p.Type), "model.Context") {
					refsContext = true
				}
			}
			if !refsContext {
				continue
			}
			discovered++
			if !coveredSurfaces[fn.Name.Name] {
				t.Errorf("output surface %q renders a model.Context but is not covered by the safety-guard "+
					"coverage test — add it to coveredSurfaces and assert its guard in "+
					"TestSafety_everySurfaceCarriesGuards, or a destructive warning can ship dropped here.", fn.Name.Name)
			}
		}
	}
	// Guard against a vacuous pass (a broken scan that discovers nothing).
	if discovered < 5 {
		t.Fatalf("surface scan found only %d renderers — expected ≥5 (JSON/SARIF/JUnit/Terminal/Prometheus…); the AST scan is broken", discovered)
	}
}

// typeString renders a small subset of type expressions (enough for io.Writer,
// *model.Context, []*model.Context) to a string.
func typeString(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.SelectorExpr:
		return typeString(t.X) + "." + t.Sel.Name
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return "*" + typeString(t.X)
	case *ast.ArrayType:
		return "[]" + typeString(t.Elt)
	}
	return ""
}
