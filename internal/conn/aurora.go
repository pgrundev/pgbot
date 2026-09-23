package conn

// Aurora instance discovery for `pgbot inspect --all-instances`. An Aurora
// cluster endpoint stands for several DB instances (one writer, N readers), and
// pg_stat_* on one of them says nothing about the others. Discovery needs only
// what the issue asked for — SQL and DNS: aurora_replica_status() lists the
// members, and every instance endpoint is derivable from the cluster endpoint's
// name. No AWS credentials, CLI, SDK, or RDS API.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
)

// AuroraInstance is one DB instance of an Aurora cluster.
type AuroraInstance struct {
	ID     string // DB instance identifier, e.g. "prod-db-instance-1"
	Writer bool   // the current writer (aurora_replica_status: session_id = MASTER_SESSION_ID)
	Host   string // the instance endpoint to reach it at; see AuroraInstanceHost
}

// Role is what Server.InstanceRole reports.
func (i AuroraInstance) Role() string {
	if i.Writer {
		return "writer"
	}
	return "reader"
}

// auroraStatusRow is the part of aurora_replica_status() discovery reads.
type auroraStatusRow struct {
	ServerID  string
	SessionID string
}

// AuroraInstances lists the cluster's members: writer first, then readers by ID.
// It refuses anything that is not Aurora rather than guessing.
func AuroraInstances(ctx context.Context, t *Target) ([]AuroraInstance, error) {
	if t.Caps.Provider != ProviderAurora {
		return nil, errors.New("not an Aurora cluster (aurora_version() is missing) — --all-instances needs a native Aurora endpoint")
	}
	rows, err := t.Pool.Query(ctx, `SELECT server_id, coalesce(session_id, '') FROM aurora_replica_status() ORDER BY server_id`)
	if err != nil {
		return nil, fmt.Errorf("aurora_replica_status(): %w", err)
	}
	defer rows.Close()
	var status []auroraStatusRow
	for rows.Next() {
		var r auroraStatusRow
		if err := rows.Scan(&r.ServerID, &r.SessionID); err != nil {
			return nil, err
		}
		status = append(status, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return auroraInstancesFromStatus(status)
}

// auroraInstancesFromStatus turns aurora_replica_status() rows into instances.
// Exactly one writer is expected; anything else is a topology pgbot does not
// understand (a failover in flight, a global-database secondary) and is reported
// rather than inspected under a wrong role.
func auroraInstancesFromStatus(status []auroraStatusRow) ([]AuroraInstance, error) {
	var out []AuroraInstance
	writers := 0
	for _, r := range status {
		id := strings.TrimSpace(r.ServerID)
		if id == "" {
			continue
		}
		inst := AuroraInstance{ID: id, Writer: r.SessionID == "MASTER_SESSION_ID"}
		if inst.Writer {
			writers++
		}
		out = append(out, inst)
	}
	if len(out) == 0 {
		return nil, errors.New("aurora_replica_status() listed no instances")
	}
	if writers != 1 {
		return nil, fmt.Errorf("aurora_replica_status() reports %d writers among %d instances; expected exactly one — refusing to guess roles", writers, len(out))
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Writer != out[j].Writer {
			return out[i].Writer
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// AuroraInstanceIdentifier returns the identifier of the instance this Target is
// connected to — the check that a derived endpoint reached the member it names.
func AuroraInstanceIdentifier(ctx context.Context, t *Target) (string, error) {
	var id string
	if err := t.Pool.QueryRow(ctx, `SELECT aurora_db_instance_identifier()`).Scan(&id); err != nil {
		return "", fmt.Errorf("cannot verify the instance identity (aurora_db_instance_identifier()): %w", err)
	}
	return id, nil
}

// AuroraInstanceHost derives an instance endpoint from a cluster, reader, custom,
// or instance endpoint of the same cluster. RDS names are
//
//	<name>.<label>.<region>.rds.amazonaws.com[.cn]
//
// where <label> is cluster-<id>, cluster-ro-<id>, cluster-custom-<id>, or, for an
// instance endpoint, the bare <id>; every instance lives at <instance>.<id>.<rest>.
// An RDS Proxy (proxy-<id>) hides the members and is refused, as is anything
// that is not an RDS name — the caller may resolve a custom CNAME first.
func AuroraInstanceHost(endpoint, instanceID string) (string, error) {
	host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(endpoint), "."))
	instanceID = strings.ToLower(strings.TrimSpace(instanceID))
	if instanceID == "" {
		return "", errors.New("empty Aurora instance identifier")
	}
	if net.ParseIP(host) != nil {
		return "", fmt.Errorf("%s is an IP address; --all-instances derives instance endpoints from the cluster endpoint's DNS name", endpoint)
	}
	labels := strings.Split(host, ".")
	if len(labels) < 6 || labels[3] != "rds" || labels[4] != "amazonaws" || labels[5] != "com" {
		return "", fmt.Errorf("%s is not an RDS endpoint (<name>.<id>.<region>.rds.amazonaws.com); pass the cluster endpoint, or a name that resolves to it", endpoint)
	}
	label := labels[1]
	switch {
	case strings.HasPrefix(label, "proxy-"):
		return "", fmt.Errorf("%s is an RDS Proxy endpoint, which hides the instances behind it; pass the cluster endpoint", endpoint)
	case strings.HasPrefix(label, "cluster-ro-"):
		label = strings.TrimPrefix(label, "cluster-ro-")
	case strings.HasPrefix(label, "cluster-custom-"):
		label = strings.TrimPrefix(label, "cluster-custom-")
	case strings.HasPrefix(label, "cluster-"):
		label = strings.TrimPrefix(label, "cluster-")
	}
	if label == "" {
		return "", fmt.Errorf("%s has an empty cluster id label", endpoint)
	}
	return strings.Join(append([]string{instanceID, label}, labels[2:]...), "."), nil
}

// CanonicalRDSHost returns endpoint if it is already an RDS name, otherwise the
// RDS name its CNAME chain ends at (a custom domain in front of the cluster
// endpoint). Anything else is an error: the DNS name is the only thing instance
// endpoints can be derived from, and guessing is off the table.
func CanonicalRDSHost(endpoint string) (string, error) {
	if _, err := AuroraInstanceHost(endpoint, "probe"); err == nil {
		return strings.ToLower(strings.TrimSuffix(endpoint, ".")), nil
	}
	cname, err := net.LookupCNAME(endpoint)
	if err != nil {
		return "", fmt.Errorf("%s is not an RDS endpoint and has no CNAME to one: %w", endpoint, err)
	}
	cname = strings.ToLower(strings.TrimSuffix(cname, "."))
	if _, err := AuroraInstanceHost(cname, "probe"); err != nil {
		return "", fmt.Errorf("%s resolves to %s, which is not an RDS cluster endpoint", endpoint, cname)
	}
	return cname, nil
}
