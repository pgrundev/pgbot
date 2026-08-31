// Package crdbhttp reads CockroachDB's HTTP health surfaces. It deliberately
// keeps authentication and TLS material outside the public diagnostic model.
package crdbhttp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	apiSessionHeader = "X-Cockroach-API-Session"
	maxResponseBytes = 32 << 20
	maxTableMetadata = 500
)

// Config is the optional HTTP side of a CockroachDB inspection. AdminURL is
// the DB Console/API origin (for example https://host:26258). PrometheusURL may
// be an exact /_status/load URL or just an origin; when empty it is derived from
// AdminURL. APISession is useful when the SQL login has no password; otherwise
// Client obtains a short-lived API session from the SQL credentials.
type Config struct {
	AdminURL      string
	PrometheusURL string
	APISession    string
	Username      string
	Password      string
	HTTPClient    *http.Client
}

// Client is safe for sequential or concurrent use. A session created by this
// client is revoked by Close; a caller-provided session is never revoked.
type Client struct {
	admin *url.URL
	prom  *url.URL
	http  *http.Client
	user  string
	pass  string

	mu           sync.Mutex
	session      string
	ownedSession bool
}

// NewFromConnectionString builds the HTTP client using the same CA/client
// certificate material and credentials as the pgwire connection. No secret is
// retained in a model or emitted in an error.
func NewFromConnectionString(connString string, cfg Config) (*Client, error) {
	pgcfg, err := pgxpool.ParseConfig(connString)
	if err != nil {
		return nil, fmt.Errorf("parse connection string for CockroachDB HTTP: %w", err)
	}
	if cfg.Username == "" {
		cfg.Username = pgcfg.ConnConfig.User
	}
	if cfg.Password == "" {
		cfg.Password = pgcfg.ConnConfig.Password
	}
	if cfg.HTTPClient == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		if pgcfg.ConnConfig.TLSConfig != nil {
			transport.TLSClientConfig = pgcfg.ConnConfig.TLSConfig.Clone()
			// net/http fills ServerName from each request host when this is empty,
			// allowing an explicitly separate Prometheus host to verify correctly.
			transport.TLSClientConfig.ServerName = ""
		}
		cfg.HTTPClient = &http.Client{Transport: transport, Timeout: 15 * time.Second}
	}

	admin, err := parseOptionalURL(cfg.AdminURL, false)
	if err != nil {
		return nil, fmt.Errorf("CockroachDB admin URL: %w", err)
	}
	promRaw := cfg.PrometheusURL
	if promRaw == "" && admin != nil {
		promRaw = endpoint(admin, "/_status/load").String()
	}
	prom, err := parseOptionalURL(promRaw, true)
	if err != nil {
		return nil, fmt.Errorf("CockroachDB Prometheus URL: %w", err)
	}
	return &Client{
		admin: admin, prom: prom, http: cfg.HTTPClient, user: cfg.Username, pass: cfg.Password,
		session: cfg.APISession,
	}, nil
}

func parseOptionalURL(raw string, appendLoad bool) (*url.URL, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("must be an absolute http(s) URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	if appendLoad && (u.Path == "" || u.Path == "/") {
		u = endpoint(u, "/_status/load")
	}
	return u, nil
}

func endpoint(base *url.URL, path string) *url.URL {
	u := *base
	u.Path = strings.TrimRight(base.Path, "/") + path
	u.RawQuery = ""
	u.Fragment = ""
	return &u
}

func (c *Client) HasAdmin() bool      { return c != nil && c.admin != nil }
func (c *Client) HasPrometheus() bool { return c != nil && c.prom != nil }

type Locality struct {
	Tiers []struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	} `json:"tiers"`
}

func (l Locality) String() string {
	parts := make([]string, 0, len(l.Tiers))
	for _, t := range l.Tiers {
		parts = append(parts, t.Key+"="+t.Value)
	}
	return strings.Join(parts, ",")
}

// Node is the stable subset of /api/v2/nodes used by the health collector.
type Node struct {
	NodeID            int                           `json:"node_id"`
	Locality          Locality                      `json:"locality"`
	BuildTag          string                        `json:"build_tag"`
	StartedAt         int64                         `json:"started_at"`
	UpdatedAt         int64                         `json:"updated_at"`
	LivenessStatus    int                           `json:"liveness_status"`
	Metrics           map[string]float64            `json:"metrics"`
	StoreMetrics      map[string]map[string]float64 `json:"store_metrics"`
	TotalSystemMemory int64                         `json:"total_system_memory"`
	NumCPUs           int                           `json:"num_cpus"`
	NumVCPUs          float64                       `json:"num_vcpus"`
}

type nodesResponse struct {
	Nodes []Node `json:"nodes"`
	Next  int    `json:"next"`
}

// Nodes returns all node-status pages.
func (c *Client) Nodes(ctx context.Context) ([]Node, error) {
	if !c.HasAdmin() {
		return nil, errors.New("CockroachDB admin endpoint not configured")
	}
	var all []Node
	offset := 0
	for page := 0; page < 100; page++ {
		u := endpoint(c.admin, "/api/v2/nodes/")
		q := u.Query()
		q.Set("limit", "100")
		if offset > 0 {
			q.Set("offset", strconv.Itoa(offset))
		}
		u.RawQuery = q.Encode()
		var out nodesResponse
		if err := c.getJSON(ctx, u, &out); err != nil {
			return all, err
		}
		all = append(all, out.Nodes...)
		if out.Next == 0 || out.Next == offset {
			return all, nil
		}
		offset = out.Next
	}
	return all, errors.New("CockroachDB nodes API pagination exceeded 100 pages")
}

// HotRange is the stable subset of /api/v2/ranges/hot used by pgbot.
type HotRange struct {
	RangeID             int64    `json:"range_id"`
	NodeID              int      `json:"node_id"`
	StoreID             int      `json:"store_id"`
	QPS                 float64  `json:"qps"`
	WritesPerSecond     float64  `json:"writes_per_second"`
	ReadsPerSecond      float64  `json:"reads_per_second"`
	WriteBytesPerSecond float64  `json:"write_bytes_per_second"`
	ReadBytesPerSecond  float64  `json:"read_bytes_per_second"`
	CPUTimePerSecond    float64  `json:"cpu_time_per_second"`
	LeaseholderNodeID   int      `json:"leaseholder_node_id"`
	Databases           []string `json:"databases"`
	Tables              []string `json:"tables"`
	Indexes             []string `json:"indexes"`
	SchemaName          string   `json:"schema_name"`
	ReplicaNodeIDs      []int    `json:"replica_node_ids"`
}

type hotRangesResponse struct {
	Ranges []HotRange `json:"ranges"`
	Errors []struct {
		ErrorMessage string `json:"error_message"`
		NodeID       int    `json:"node_id"`
	} `json:"response_error"`
	Next string `json:"next"`
}

// HotRanges returns the hottest replicas from every node. The endpoint pages
// by node; callers should deduplicate range IDs before ranking cluster-wide.
func (c *Client) HotRanges(ctx context.Context) ([]HotRange, error) {
	if !c.HasAdmin() {
		return nil, errors.New("CockroachDB admin endpoint not configured")
	}
	var all []HotRange
	start := ""
	var partial []error
	for page := 0; page < 100; page++ {
		u := endpoint(c.admin, "/api/v2/ranges/hot/")
		q := u.Query()
		q.Set("limit", "100")
		if start != "" {
			q.Set("start", start)
		}
		u.RawQuery = q.Encode()
		var out hotRangesResponse
		if err := c.getJSON(ctx, u, &out); err != nil {
			return all, errors.Join(append(partial, err)...)
		}
		all = append(all, out.Ranges...)
		for _, e := range out.Errors {
			partial = append(partial, fmt.Errorf("node %d: %s", e.NodeID, e.ErrorMessage))
		}
		if out.Next == "" || out.Next == start {
			return all, errors.Join(partial...)
		}
		start = out.Next
	}
	return all, errors.Join(append(partial, errors.New("CockroachDB hot-ranges API pagination exceeded 100 pages"))...)
}

// DatabaseMetadata is the stable subset of CockroachDB's cached database
// metadata used to scope and summarize a table inspection.
type DatabaseMetadata struct {
	DatabaseID int64      `json:"db_id"`
	Name       string     `json:"db_name"`
	SizeBytes  int64      `json:"size_bytes"`
	TableCount int64      `json:"table_count"`
	StoreIDs   []int64    `json:"store_ids"`
	UpdatedAt  *time.Time `json:"last_updated"`
}

// TableMetadata is a cached, data-free table health record. Byte and range
// values are produced by CockroachDB's table metadata update job; they are not
// live reads of application rows.
type TableMetadata struct {
	DatabaseID           int64      `json:"db_id"`
	Database             string     `json:"db_name"`
	TableID              int64      `json:"table_id"`
	Schema               string     `json:"schema_name"`
	Table                string     `json:"table_name"`
	ReplicationSizeBytes int64      `json:"replication_size_bytes"`
	RangeCount           int64      `json:"range_count"`
	ColumnCount          int64      `json:"column_count"`
	IndexCount           int64      `json:"index_count"`
	PercentLiveData      float64    `json:"percent_live_data"`
	TotalLiveDataBytes   int64      `json:"total_live_data_bytes"`
	TotalDataBytes       int64      `json:"total_data_bytes"`
	StoreIDs             []int64    `json:"store_ids"`
	AutoStatsEnabled     bool       `json:"auto_stats_enabled"`
	StatsLastUpdated     *time.Time `json:"stats_last_updated"`
	LastUpdateError      string     `json:"last_update_error"`
	LastUpdated          time.Time  `json:"last_updated"`
	ReplicaCount         int64      `json:"replica_count"`
}

// TableMetadataSnapshot is deliberately bounded to the largest 500 tables in
// one database. TotalTables makes truncation visible to callers.
type TableMetadataSnapshot struct {
	Database    DatabaseMetadata
	Tables      []TableMetadata
	TotalTables int64
}

type databaseMetadataResponse struct {
	Results []DatabaseMetadata `json:"results"`
}

type tableMetadataResponse struct {
	Results        []TableMetadata `json:"results"`
	PaginationInfo struct {
		TotalResults int64 `json:"total_results"`
	} `json:"pagination_info"`
}

// TableMetadata returns the cached metadata for the named database, ordered by
// replicated disk estimate. The API's name filter is substring-based, so this
// method requires an exact name match before using the descriptor ID.
func (c *Client) TableMetadata(ctx context.Context, database string) (TableMetadataSnapshot, error) {
	if !c.HasAdmin() {
		return TableMetadataSnapshot{}, errors.New("CockroachDB admin endpoint not configured")
	}
	databaseURL := endpoint(c.admin, "/api/v2/database_metadata/")
	q := databaseURL.Query()
	q.Set("name", database)
	q.Set("pageSize", strconv.Itoa(maxTableMetadata))
	q.Set("sortBy", "name")
	databaseURL.RawQuery = q.Encode()
	var databases databaseMetadataResponse
	if err := c.getJSON(ctx, databaseURL, &databases); err != nil {
		return TableMetadataSnapshot{}, err
	}
	var selected *DatabaseMetadata
	for i := range databases.Results {
		if databases.Results[i].Name == database {
			selected = &databases.Results[i]
			break
		}
	}
	if selected == nil {
		return TableMetadataSnapshot{}, fmt.Errorf("database %q absent from CockroachDB metadata cache", database)
	}

	tablesURL := endpoint(c.admin, "/api/v2/table_metadata/")
	q = tablesURL.Query()
	q.Set("dbId", strconv.FormatInt(selected.DatabaseID, 10))
	q.Set("pageSize", strconv.Itoa(maxTableMetadata))
	q.Set("pageNum", "1")
	q.Set("sortBy", "replicationSize")
	q.Set("sortOrder", "desc")
	tablesURL.RawQuery = q.Encode()
	var tables tableMetadataResponse
	if err := c.getJSON(ctx, tablesURL, &tables); err != nil {
		return TableMetadataSnapshot{}, err
	}
	return TableMetadataSnapshot{Database: *selected, Tables: tables.Results, TotalTables: tables.PaginationInfo.TotalResults}, nil
}

// Load is one node's lightweight /_status/load sample.
type Load struct {
	NodeID         int
	CPUUserNS      float64
	CPUSystemNS    float64
	CPUNowNS       float64
	UptimeSeconds  float64
	SQLConnections float64
	NewConnections float64
	QueryCount     float64
}

// Loads asks the configured gateway to proxy the lightweight Prometheus scrape
// to each node. Partial rows are returned with a joined error.
func (c *Client) Loads(ctx context.Context, nodeIDs []int) ([]Load, error) {
	if !c.HasPrometheus() {
		return nil, errors.New("CockroachDB Prometheus endpoint not configured")
	}
	if len(nodeIDs) == 0 {
		nodeIDs = []int{0}
	}
	var out []Load
	var errs []error
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, id := range nodeIDs {
		id := id
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			load, err := c.load(ctx, id)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
			} else {
				out = append(out, load)
			}
		}()
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out, errors.Join(errs...)
}

func (c *Client) load(ctx context.Context, id int) (Load, error) {
	u := *c.prom
	if id > 0 {
		q := u.Query()
		q.Set("remote_node_id", strconv.Itoa(id))
		u.RawQuery = q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Load{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return Load{}, fmt.Errorf("node %d Prometheus load: %w", id, err)
	}
	defer resp.Body.Close()
	load, readErr := parseLoad(io.LimitReader(resp.Body, maxResponseBytes), id)
	if resp.StatusCode/100 != 2 {
		readErr = fmt.Errorf("HTTP %s", resp.Status)
	}
	if readErr != nil {
		return Load{}, fmt.Errorf("node %d Prometheus load: %w", id, readErr)
	}
	return load, nil
}

func parseLoad(r io.Reader, requestedNodeID int) (Load, error) {
	load := Load{NodeID: requestedNodeID}
	s := bufio.NewScanner(r)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := fields[0]
		base := name
		if i := strings.IndexByte(base, '{'); i >= 0 {
			labels := base[i+1 : len(base)-1]
			base = base[:i]
			if load.NodeID == 0 {
				for _, label := range strings.Split(labels, ",") {
					if strings.HasPrefix(label, "node_id=\"") {
						v := strings.Trim(strings.TrimPrefix(label, "node_id="), "\"")
						load.NodeID, _ = strconv.Atoi(v)
					}
				}
			}
		}
		v, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil {
			continue
		}
		switch base {
		case "sys_cpu_user_ns":
			load.CPUUserNS = v
		case "sys_cpu_sys_ns":
			load.CPUSystemNS = v
		case "sys_cpu_now_ns":
			load.CPUNowNS = v
		case "sys_uptime":
			load.UptimeSeconds = v
		case "sql_conns":
			load.SQLConnections = v
		case "sql_new_conns":
			load.NewConnections = v
		case "sql_query_count":
			load.QueryCount = v
		}
	}
	if err := s.Err(); err != nil {
		return load, err
	}
	if load.CPUNowNS == 0 && load.QueryCount == 0 && load.SQLConnections == 0 {
		return load, errors.New("no recognized load metrics")
	}
	return load, nil
}

func (c *Client) getJSON(ctx context.Context, u *url.URL, out any) error {
	session, err := c.ensureSession(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set(apiSessionHeader, session)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("GET %s: HTTP %s", u.Path, resp.Status)
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes))
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("decode %s: %w", u.Path, err)
	}
	return nil
}

func (c *Client) ensureSession(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session != "" {
		return c.session, nil
	}
	if c.admin == nil {
		return "", errors.New("CockroachDB admin endpoint not configured")
	}
	if c.user == "" || c.pass == "" {
		return "", errors.New("admin API needs a SQL username/password or PGBOT_CRDB_API_SESSION")
	}
	form := url.Values{"username": {c.user}, "password": {c.pass}}
	u := endpoint(c.admin, "/api/v2/login/")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("CockroachDB API login: HTTP %s", resp.Status)
	}
	var body struct {
		Session string `json:"session"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", fmt.Errorf("decode CockroachDB API login: %w", err)
	}
	if body.Session == "" {
		return "", errors.New("CockroachDB API login returned no session")
	}
	c.session = body.Session
	c.ownedSession = true
	return c.session, nil
}

// Close revokes only a session created by this client.
func (c *Client) Close(ctx context.Context) error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.ownedSession || c.session == "" || c.admin == nil {
		return nil
	}
	u := endpoint(c.admin, "/api/v2/logout/")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(nil))
	if err != nil {
		return err
	}
	req.Header.Set(apiSessionHeader, c.session)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("CockroachDB API logout: HTTP %s", resp.Status)
	}
	c.session = ""
	c.ownedSession = false
	return nil
}
