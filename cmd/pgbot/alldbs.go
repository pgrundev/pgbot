package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"

	"github.com/pgrundev/pgbot/internal/collect"
	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/findings"
	"github.com/pgrundev/pgbot/internal/model"
	"github.com/pgrundev/pgbot/internal/render"
	"github.com/pgrundev/pgbot/internal/store"
)

// runInspectAll inspects every connectable, non-template database in the cluster
// (B3). Cluster-wide findings (settings, replication, archiving, wraparound,
// cluster activity) are computed on every connection but reported ONCE — marked
// on the first database and dropped from the rest — so they aren't repeated N
// times. Connections are serial by default; --parallel caps concurrency, because
// opening N connections to a cluster that already fired connection_saturation
// would be the wrong default.
func runInspectAll(ctx context.Context, connString string, f inspectFlags) error {
	dbs, err := listAllDatabases(ctx, connString)
	if err != nil {
		return fmt.Errorf("list databases: %s", conn.RedactConnString(err.Error()))
	}
	if len(dbs) == 0 {
		return fmt.Errorf("no connectable databases found")
	}

	contexts := make([]*model.Context, len(dbs))
	workers := f.parallel
	if workers < 1 {
		workers = 1
	}
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	for i, db := range dbs {
		wg.Add(1)
		go func(i int, db string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			c, err := inspectOne(ctx, connString, db, f)
			if err != nil {
				fmt.Fprintf(os.Stderr, "pgbot: skipping %s: %s\n", db, conn.RedactConnString(err.Error()))
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				return
			}
			contexts[i] = c
		}(i, db)
	}
	wg.Wait()

	// Keep only the databases that inspected, in cluster order.
	var out []*model.Context
	for _, c := range contexts {
		if c != nil {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return fmt.Errorf("every database failed to inspect: %w", firstErr)
	}

	// Fingerprints must be distinct per database (P0-1) — assert it, because this
	// is the mode that actually exercises many databases on one server.
	if fp := duplicateFingerprint(out); fp != "" {
		return fmt.Errorf("internal: two databases share fingerprint %s — baselines would collide", fp)
	}

	dedupeClusterWide(out)

	worst := exitClean
	for _, c := range out {
		if code := exitCode(c.Findings, f.failOn); code > worst {
			worst = code
		}
	}

	if err := renderAll(out, f); err != nil {
		return err
	}
	os.Exit(worst)
	return nil
}

// inspectOne runs the full read-only pipeline for one database and returns its
// Context (no rendering, no exit).
func inspectOne(ctx context.Context, connString, database string, f inspectFlags) (*model.Context, error) {
	target, err := conn.ConnectDB(ctx, connString, database)
	if err != nil {
		return nil, err
	}
	defer target.Close()
	httpClient, err := newCockroachHTTP(connString, target.Caps, f)
	if err != nil {
		return nil, err
	}
	defer closeCockroachHTTP(httpClient)

	c, err := collect.Run(ctx, target, collect.Options{
		Interval: f.interval, RawQueryText: f.rawQueries, ASHHz: f.ashHz, ASHWindow: f.window, Deadline: f.timeout,
		SchemaOnly: f.schemaProfile(), CockroachHTTP: httpClient,
	})
	if err != nil {
		return nil, err
	}
	c.Server.ViaPooler = target.Pooler.Detected
	host, port := hostPort(target)
	c.Fingerprint = store.Fingerprint(host, port, c.Server.Database, target.Caps.SystemIdentifier)
	if !f.noStore {
		withStore(f.storePath, c)
	}
	if err := computeFindings(c, f); err != nil {
		return nil, err
	}
	return c, nil
}

// listAllDatabases returns the connectable, non-template databases, sorted.
func listAllDatabases(ctx context.Context, connString string) ([]string, error) {
	target, err := conn.Connect(ctx, connString)
	if err != nil {
		return nil, err
	}
	defer target.Close()
	rows, err := target.Pool.Query(ctx, `SELECT datname FROM pg_database WHERE datallowconn AND NOT datistemplate ORDER BY datname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var dbs []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		dbs = append(dbs, d)
	}
	return dbs, rows.Err()
}

// dedupeClusterWide keeps the FIRST occurrence of each cluster-wide finding and
// removes the rest, so it's reported once (B3). First occurrence, not "the first
// database's copy": if database 0's collection missed the finding (permissions,
// a per-connection timeout), stripping it from every later context would erase
// a live report — for checksum_failures that's a corruption finding vanishing
// from the output and the exit code.
func dedupeClusterWide(contexts []*model.Context) {
	seen := map[string]bool{}
	for _, c := range contexts {
		kept := c.Findings[:0]
		for j := range c.Findings {
			fd := c.Findings[j]
			if findings.ClusterWide(fd.ID) {
				if seen[fd.ID] {
					continue
				}
				seen[fd.ID] = true
				fd.ClusterScoped = true
			}
			kept = append(kept, fd)
		}
		c.Findings = kept
	}
}

// duplicateFingerprint returns a fingerprint shared by two contexts, or "".
func duplicateFingerprint(contexts []*model.Context) string {
	seen := map[string]bool{}
	for _, c := range contexts {
		if seen[c.Fingerprint] {
			return c.Fingerprint
		}
		seen[c.Fingerprint] = true
	}
	return ""
}

// renderAll writes the multi-database output in the requested format.
func renderAll(contexts []*model.Context, f inspectFlags) error {
	switch f.format {
	case "json":
		// A top-level array; each entry is the existing (additive) Context shape.
		return json.NewEncoder(os.Stdout).Encode(contexts)
	case "sarif":
		return render.SARIF(os.Stdout, mergeContexts(contexts))
	case "junit":
		return render.JUnit(os.Stdout, mergeContexts(contexts), f.failOn)
	case "prometheus":
		// One grouped exposition for all databases — repeated # HELP/# TYPE lines
		// (one block per database) would be rejected by the textfile collector.
		return render.PrometheusAll(os.Stdout, contexts)
	default:
		for i, c := range contexts {
			if i > 0 {
				fmt.Fprintln(os.Stdout)
			}
			fmt.Fprintf(os.Stdout, "═══ database: %s ═══\n", c.Server.Database)
			opts := render.Options{Color: useColor(f.noColor), Width: terminalWidth(), Full: f.full, Host: c.Server.Database}
			if err := render.Terminal(os.Stdout, c, opts); err != nil {
				return err
			}
		}
		return nil
	}
}

// mergeContexts flattens per-database findings into one Context for the SARIF/JUnit
// aggregate, prefixing each finding's object with its database so entries from
// different databases stay distinct.
func mergeContexts(contexts []*model.Context) *model.Context {
	merged := &model.Context{SchemaVersion: model.SchemaVersion}
	for _, c := range contexts {
		db := c.Server.Database
		for _, fd := range c.Findings {
			if !fd.ClusterScoped {
				if fd.Object == "" {
					fd.Object = "db:" + db
				} else {
					fd.Object = "db:" + db + "/" + fd.Object
				}
			}
			merged.Findings = append(merged.Findings, fd)
		}
	}
	sort.SliceStable(merged.Findings, func(i, j int) bool {
		return merged.Findings[i].ID < merged.Findings[j].ID
	})
	return merged
}
