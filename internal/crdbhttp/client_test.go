package crdbhttp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestClientAdminPrometheusAndSessionLifecycle(t *testing.T) {
	var mu sync.Mutex
	login, logout := 0, 0
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/api/v2/login/":
			login++
			if r.FormValue("username") != "monitor" || r.FormValue("password") != "secret" {
				t.Fatalf("unexpected login credentials")
			}
			fmt.Fprint(w, `{"session":"temporary"}`)
		case "/api/v2/logout/":
			logout++
			if r.Header.Get(apiSessionHeader) != "temporary" {
				t.Errorf("logout lacks session")
			}
			fmt.Fprint(w, `{}`)
		case "/api/v2/nodes/":
			if r.Header.Get(apiSessionHeader) != "temporary" {
				t.Errorf("nodes lacks session")
			}
			fmt.Fprint(w, `{"nodes":[{"node_id":1,"build_tag":"v25.2.1","liveness_status":3,"store_metrics":{"1":{"capacity":100,"capacity.available":40,"ranges":9}}}]}`)
		case "/api/v2/ranges/hot/":
			if r.Header.Get(apiSessionHeader) != "temporary" {
				t.Errorf("hot ranges lacks session")
			}
			fmt.Fprint(w, `{"ranges":[{"range_id":7,"node_id":1,"store_id":1,"qps":12,"cpu_time_per_second":250000000}]}`)
		case "/api/v2/database_metadata/":
			if r.URL.Query().Get("name") != "app" {
				t.Errorf("database metadata filter = %q", r.URL.RawQuery)
			}
			fmt.Fprint(w, `{"results":[{"db_id":50,"db_name":"app","size_bytes":4096,"table_count":1,"store_ids":[1],"last_updated":"2026-08-28T10:00:00Z"}],"pagination_info":{"total_results":1,"page_size":500,"page_num":1}}`)
		case "/api/v2/table_metadata/":
			if r.URL.Query().Get("dbId") != "50" || r.URL.Query().Get("pageSize") != "500" || r.URL.Query().Get("sortBy") != "replicationSize" {
				t.Errorf("table metadata query = %q", r.URL.RawQuery)
			}
			fmt.Fprint(w, `{"results":[{"db_id":50,"db_name":"app","table_id":51,"schema_name":"public","table_name":"orders","replication_size_bytes":4096,"range_count":3,"column_count":5,"index_count":2,"percent_live_data":0.75,"total_live_data_bytes":3000,"total_data_bytes":4000,"store_ids":[1,2,3],"auto_stats_enabled":true,"stats_last_updated":"2026-08-28T09:00:00Z","last_update_error":null,"last_updated":"2026-08-28T10:00:00Z","replica_count":9}],"pagination_info":{"total_results":1,"page_size":500,"page_num":1}}`)
		case "/_status/load":
			id, _ := strconv.Atoi(r.URL.Query().Get("remote_node_id"))
			fmt.Fprintf(w, "sys_cpu_now_ns %d\nsql_conns %d\nsql_new_conns %d\nsql_query_count %d\n", 100+id, 5+id, 20+id, 1000+id)
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	c, err := NewFromConnectionString("postgres://monitor:secret@localhost:26257/defaultdb?sslmode=disable", Config{AdminURL: srv.URL, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := c.Nodes(context.Background())
	if err != nil || len(nodes) != 1 || nodes[0].StoreMetrics["1"]["capacity.available"] != 40 {
		t.Fatalf("nodes=%+v err=%v", nodes, err)
	}
	hot, err := c.HotRanges(context.Background())
	if err != nil || len(hot) != 1 || hot[0].RangeID != 7 {
		t.Fatalf("hot=%+v err=%v", hot, err)
	}
	tables, err := c.TableMetadata(context.Background(), "app")
	if err != nil || tables.Database.SizeBytes != 4096 || tables.TotalTables != 1 || len(tables.Tables) != 1 || tables.Tables[0].PercentLiveData != .75 || tables.Tables[0].LastUpdateError != "" {
		t.Fatalf("tables=%+v err=%v", tables, err)
	}
	loads, err := c.Loads(context.Background(), []int{1, 2})
	if err != nil || len(loads) != 2 || loads[1].NodeID != 2 || loads[1].QueryCount != 1002 {
		t.Fatalf("loads=%+v err=%v", loads, err)
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if login != 1 || logout != 1 {
		t.Fatalf("login=%d logout=%d", login, logout)
	}
}

func TestParseLoadUsesNodeLabel(t *testing.T) {
	l, err := parseLoad(strings.NewReader("sql_query_count{node_id=\"9\"} 12\nsql_conns{node_id=\"9\"} 3\n"), 0)
	if err != nil || l.NodeID != 9 || l.QueryCount != 12 {
		t.Fatalf("load=%+v err=%v", l, err)
	}
}
