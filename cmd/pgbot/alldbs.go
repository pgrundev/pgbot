package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/pgrundev/pgbot/internal/collect"
	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/findings"
	"github.com/pgrundev/pgbot/internal/model"
	"github.com/pgrundev/pgbot/internal/render"
	"github.com/pgrundev/pgbot/internal/store"
)

// inspectTarget is one (cluster member, database) pair the fan-out inspects.
// Without --all-instances the member is nil and the target is just a database;
// without --all-databases the database is "" — the connString's own.
type inspectTarget struct {
	instance *conn.AuroraInstance
	database string
}

// label names the target in diagnostics.
func (t inspectTarget) label() string {
	db := t.database
	if db == "" {
		db = "the connection's database"
	}
	if t.instance == nil {
		return db
	}
	return fmt.Sprintf("instance %s (%s), %s", t.instance.ID, t.instance.Role(), db)
}

// runInspectAll inspects every target of the fan-out: every connectable,
// non-template database (--all-databases, B3), every Aurora cluster member
// (--all-instances), or their cross product. Cluster-wide findings (settings,
// replication, archiving, wraparound, cluster activity) are computed on every
// connection but reported ONCE per server — marked on the first database and
// dropped from the rest — so they aren't repeated N times; under --all-instances
// each member is its own server and keeps its own copy, since parameter groups
// and replication state differ per instance. Connections are serial by default;
// --parallel caps concurrency, because opening N connections to a cluster that
// already fired connection_saturation would be the wrong default.
func runInspectAll(ctx context.Context, connString string, f inspectFlags) error {
	targets, err := planTargets(ctx, connString, f)
	if err != nil {
		return err
	}

	contexts := make([]*model.Context, len(targets))
	workers := f.parallel
	if workers < 1 {
		workers = 1
	}
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	skipped := 0
	for i, t := range targets {
		wg.Add(1)
		go func(i int, t inspectTarget) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			c, err := inspectOne(ctx, connString, t, f)
			if err != nil {
				fmt.Fprintf(os.Stderr, "pgbot: skipping %s: %s\n", t.label(), conn.RedactConnString(err.Error()))
				mu.Lock()
				skipped++
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				return
			}
			contexts[i] = c
		}(i, t)
	}
	wg.Wait()

	// Keep only the targets that inspected, in plan order.
	var out []*model.Context
	for _, c := range contexts {
		if c != nil {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return fmt.Errorf("every target failed to inspect: %w", firstErr)
	}

	// Fingerprints must be distinct per target (P0-1) — assert it, because this
	// is the mode that actually exercises many databases (and members) at once.
	if fp := duplicateFingerprint(out); fp != "" {
		return fmt.Errorf("internal: two targets share fingerprint %s — baselines would collide", fp)
	}

	dedupeClusterWide(out)

	worst := exitClean
	for _, c := range out {
		if code := exitCode(c.Findings, f.failOn); code > worst {
			worst = code
		}
	}
	// --all-instances promises the whole cluster; anything less is partial
	// coverage, reported after the partial output and failed in the exit code so
	// a script cannot mistake "the members we could reach" for "the cluster".
	if f.allInstances && skipped > 0 {
		fmt.Fprintf(os.Stderr, "pgbot: partial cluster coverage — %d of %d targets not inspected (see above)\n", skipped, len(targets))
		worst = partialCoverageExit(worst)
	}

	if err := renderAll(out, f); err != nil {
		return err
	}
	os.Exit(worst)
	return nil
}

// partialCoverageExit is the exit code for a fan-out that missed targets: the
// findings' own code if that is already a failure, else exitFailure.
func partialCoverageExit(worst int) int {
	if worst < exitFailure {
		return exitFailure
	}
	return worst
}

// planTargets lists what the fan-out inspects, in output order.
func planTargets(ctx context.Context, connString string, f inspectFlags) ([]inspectTarget, error) {
	dbs := []string{""}
	if f.allDatabases {
		list, err := listAllDatabases(ctx, connString)
		if err != nil {
			return nil, fmt.Errorf("list databases: %s", conn.RedactConnString(err.Error()))
		}
		if len(list) == 0 {
			return nil, fmt.Errorf("no connectable databases found")
		}
		dbs = list
	}
	var instances []conn.AuroraInstance
	if f.allInstances {
		var err error
		if instances, err = discoverAuroraInstances(ctx, connString); err != nil {
			return nil, fmt.Errorf("discover instances: %s", conn.RedactConnString(err.Error()))
		}
	}
	return composeTargets(instances, dbs), nil
}

// composeTargets crosses members with databases: writer first (instances arrive
// in that order), every database of one member before the next member, so the
// output reads as one server at a time.
func composeTargets(instances []conn.AuroraInstance, dbs []string) []inspectTarget {
	var out []inspectTarget
	if len(instances) == 0 {
		for _, db := range dbs {
			out = append(out, inspectTarget{database: db})
		}
		return out
	}
	for i := range instances {
		inst := instances[i]
		for _, db := range dbs {
			out = append(out, inspectTarget{instance: &inst, database: db})
		}
	}
	return out
}

// discoverAuroraInstances asks the entry endpoint for the cluster's members and
// derives each one's instance endpoint from the entry's DNS name — SQL and DNS
// only. A custom domain in front of the cluster endpoint is followed through
// its CNAME; a proxy or a non-RDS name fails here instead of guessing.
func discoverAuroraInstances(ctx context.Context, connString string) ([]conn.AuroraInstance, error) {
	target, err := conn.Connect(ctx, connString)
	if err != nil {
		return nil, err
	}
	defer target.Close()
	instances, err := conn.AuroraInstances(ctx, target)
	if err != nil {
		return nil, err
	}
	host, _ := hostPort(target)
	endpoint, err := conn.CanonicalRDSHost(host)
	if err != nil {
		return nil, err
	}
	for i := range instances {
		if instances[i].Host, err = conn.AuroraInstanceHost(endpoint, instances[i].ID); err != nil {
			return nil, err
		}
	}
	fmt.Fprintf(os.Stderr, "pgbot: %d Aurora instance(s) behind %s\n", len(instances), endpoint)
	return instances, nil
}

// inspectOne runs the full read-only pipeline for one target and returns its
// Context (no rendering, no exit). Under --all-instances the member's identity
// is verified before anything is collected, so a derived endpoint that reached
// the wrong member is an error, never a mislabeled report.
func inspectOne(ctx context.Context, connString string, t inspectTarget, f inspectFlags) (*model.Context, error) {
	var target *conn.Target
	var err error
	if t.instance != nil {
		target, err = conn.ConnectDBAt(ctx, connString, t.database, t.instance.Host)
	} else {
		target, err = conn.ConnectDB(ctx, connString, t.database)
	}
	if err != nil {
		return nil, err
	}
	defer target.Close()
	if t.instance != nil {
		got, err := conn.AuroraInstanceIdentifier(ctx, target)
		if err != nil {
			return nil, err
		}
		if !strings.EqualFold(got, t.instance.ID) {
			return nil, fmt.Errorf("%s reached instance %q, not %q — endpoint derivation does not fit this cluster; please report the cluster endpoint's shape", t.instance.Host, got, t.instance.ID)
		}
	}

	c, err := collect.Run(ctx, target, collect.Options{
		Interval: f.interval, RawQueryText: f.rawQueries, ASHHz: f.ashHz, ASHWindow: f.window, Deadline: f.timeout,
		SchemaOnly: f.schemaProfile(),
	})
	if err != nil {
		return nil, err
	}
	c.Server.ViaPooler = target.Pooler.Detected
	if t.instance != nil {
		c.Server.Instance = t.instance.ID
		c.Server.InstanceRole = t.instance.Role()
	}
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

// dedupeClusterWide keeps the FIRST occurrence of each cluster-wide finding per
// server and removes the rest, so it's reported once (B3). First occurrence, not
// "the first database's copy": if database 0's collection missed the finding
// (permissions, a per-connection timeout), stripping it from every later context
// would erase a live report — for checksum_failures that's a corruption finding
// vanishing from the output and the exit code. Under --all-instances the key
// includes the member: each instance is its own server with its own settings.
func dedupeClusterWide(contexts []*model.Context) {
	seen := map[string]bool{}
	for _, c := range contexts {
		kept := c.Findings[:0]
		for j := range c.Findings {
			fd := c.Findings[j]
			if findings.ClusterWide(fd.ID) {
				key := c.Server.Instance + "\x00" + fd.ID
				if seen[key] {
					continue
				}
				seen[key] = true
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
			fmt.Fprintf(os.Stdout, "═══ %s ═══\n", contextHeader(c))
			opts := render.Options{Color: useColor(f.noColor), Width: terminalWidth(), Full: f.full, Host: contextScope(c)}
			if err := render.Terminal(os.Stdout, c, opts); err != nil {
				return err
			}
		}
		return nil
	}
}

// contextHeader is the text-mode banner for one target.
func contextHeader(c *model.Context) string {
	if c.Server.Instance != "" {
		return fmt.Sprintf("instance: %s (%s) · database: %s", c.Server.Instance, c.Server.InstanceRole, c.Server.Database)
	}
	return "database: " + c.Server.Database
}

// contextScope is the short target name: "db" or "instance/db".
func contextScope(c *model.Context) string {
	if c.Server.Instance != "" {
		return c.Server.Instance + "/" + c.Server.Database
	}
	return c.Server.Database
}

// mergeContexts flattens per-target findings into one Context for the SARIF/JUnit
// aggregate, prefixing each finding's object with its database (and member, under
// --all-instances) so entries from different targets stay distinct. Cluster-scoped
// findings keep their object: they were already reduced to one per server.
func mergeContexts(contexts []*model.Context) *model.Context {
	merged := &model.Context{SchemaVersion: model.SchemaVersion}
	for _, c := range contexts {
		scope := "db:" + c.Server.Database
		if c.Server.Instance != "" {
			scope = "instance:" + c.Server.Instance + "/" + scope
		}
		for _, fd := range c.Findings {
			if !fd.ClusterScoped {
				if fd.Object == "" {
					fd.Object = scope
				} else {
					fd.Object = scope + "/" + fd.Object
				}
			} else if c.Server.Instance != "" {
				// One copy per member: name the member so two instances' copies
				// of the same cluster-wide finding stay distinct in the aggregate.
				fd.Object = "instance:" + c.Server.Instance
			}
			merged.Findings = append(merged.Findings, fd)
		}
	}
	sort.SliceStable(merged.Findings, func(i, j int) bool {
		return merged.Findings[i].ID < merged.Findings[j].ID
	})
	return merged
}
