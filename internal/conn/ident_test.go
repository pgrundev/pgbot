package conn

import "testing"

func TestQuoteIdent(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"pgbot_ro", "pgbot_ro"},
		{"orders_prod2", "orders_prod2"},
		{"my-app", `"my-app"`},
		{"My App DB", `"My App DB"`},
		{"MetricsRO", `"MetricsRO"`},
		{`say"hi`, `"say""hi"`},
		{"2fast", `"2fast"`},
	} {
		if got := QuoteIdent(tc.in); got != tc.want {
			t.Errorf("QuoteIdent(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}
