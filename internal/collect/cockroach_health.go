package collect

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/crdbhttp"
	"github.com/pgrundev/pgbot/internal/model"
)

type cockroachHealthSample struct {
	Nodes    []crdbhttp.Node
	Loads    []crdbhttp.Load
	AdminErr error
	PromErr  error
}

func (healthCollector) SampleWithOptions(
	ctx context.Context, t *conn.Target, caps conn.Capabilities, opts Options,
) (any, error) {
	if !caps.IsCockroachDB() {
		return (healthCollector{}).Sample(ctx, t, caps)
	}
	s := cockroachHealthSample{}
	if opts.CockroachHTTP == nil {
		s.AdminErr = errors.New("admin endpoint not configured")
		s.PromErr = errors.New("Prometheus endpoint not configured")
		return s, nil
	}
	if opts.CockroachHTTP.HasAdmin() {
		s.Nodes, s.AdminErr = opts.CockroachHTTP.Nodes(ctx)
	} else {
		s.AdminErr = errors.New("admin endpoint not configured")
	}
	var nodeIDs []int
	for _, n := range s.Nodes {
		nodeIDs = append(nodeIDs, n.NodeID)
	}
	if opts.CockroachHTTP.HasPrometheus() {
		s.Loads, s.PromErr = opts.CockroachHTTP.Loads(ctx, nodeIDs)
	} else {
		s.PromErr = errors.New("Prometheus endpoint not configured")
	}
	return s, nil
}

func assembleCockroachHealth(c *model.Context, s sampled, dt time.Duration) {
	a, aok := s.A.(cockroachHealthSample)
	b, bok := s.B.(cockroachHealthSample)
	if !aok || !bok {
		c.Health = &model.Health{Section: unavail(s.Err, "CockroachDB health sample unavailable")}
		return
	}
	h := &model.Health{Cockroach: &model.CockroachHealth{}}
	ch := h.Cockroach

	if len(b.Nodes) > 0 {
		ch.AdminAPI = sourceSection(model.ExactnessScraped, b.AdminErr)
		assembleCockroachNodes(ch, b.Nodes)
		assembleCockroachStorage(ch, a.Nodes, b.Nodes, dt)
	} else {
		ch.AdminAPI = unavail(b.AdminErr, "admin API unavailable")
		ch.Storage.Section = unavail(b.AdminErr, "CockroachDB storage and replication metrics require the Admin API")
	}

	queriesPerSec, newConnectionsPerSec, sampledNodes := cockroachLoadRates(a.Loads, b.Loads, dt)
	if sampledNodes > 0 {
		ch.Prometheus = sourceSection(model.ExactnessSampled, errors.Join(a.PromErr, b.PromErr))
		ch.QueriesPerSec = &queriesPerSec
		ch.NewConnectionsPerSec = &newConnectionsPerSec
	} else {
		ch.Prometheus = unavail(errors.Join(a.PromErr, b.PromErr), "Prometheus load endpoint unavailable")
	}
	if len(b.Loads) > 0 {
		var conns int
		for _, l := range b.Loads {
			conns += int(l.SQLConnections)
		}
		// Only replace the API's cluster total when every reported node was
		// scraped; a partial Prometheus response must not make connections fall.
		if ch.NodesTotal == 0 || len(b.Loads) == ch.NodesTotal {
			ch.SQLConnections = conns
		}
	}

	h.Connections = ch.SQLConnections
	switch {
	case sampledNodes > 0:
		h.Section = model.Section{Exactness: model.ExactnessSampled}
	case len(b.Nodes) > 0 || len(b.Loads) > 0:
		h.Section = model.Section{Exactness: model.ExactnessScraped}
	default:
		h.Section = unavail(nil, "configure --crdb-admin-url (or PGBOT_CRDB_ADMIN_URL) for CockroachDB cluster health")
	}
	c.Health = h
}

func sourceSection(exactness string, err error) model.Section {
	s := model.Section{Exactness: exactness}
	if err != nil {
		s.Reason = conn.RedactConnString(err.Error())
	}
	return s
}

func assembleCockroachNodes(ch *model.CockroachHealth, nodes []crdbhttp.Node) {
	ch.NodesTotal = len(nodes)
	for _, n := range nodes {
		status := cockroachLivenessStatus(n.LivenessStatus)
		switch status {
		case "live":
			ch.NodesLive++
		case "draining", "decommissioning":
			ch.NodesDraining++
		case "decommissioned":
			ch.NodesDecommissioned++
		default:
			ch.NodesSuspect++
		}
		cpu := nodeCPUPercent(n.Metrics)
		rss := int64(metricValue(n.Metrics, "sys.rss"))
		memoryRatio := safeRatio(float64(rss), float64(n.TotalSystemMemory))
		conns := int(metricValue(n.Metrics, "sql.conns"))
		ch.MaxCPUPercent = max(ch.MaxCPUPercent, cpu)
		ch.MaxMemoryUsedRatio = max(ch.MaxMemoryUsedRatio, memoryRatio)
		ch.SQLConnections += conns
		ch.ServiceLatencyP99MS = max(ch.ServiceLatencyP99MS,
			metricValue(n.Metrics, "sql.service.latency-p99")/1e6)
		for _, name := range []string{
			"admission.wait_durations.cpu-p99",
			"admission.wait_durations.kv-p99",
			"admission.wait_durations.sql-kv-response-p99",
			"admission.wait_durations.sql-sql-response-p99",
		} {
			ch.AdmissionWaitP99MS = max(ch.AdmissionWaitP99MS, metricValue(n.Metrics, name)/1e6)
		}
		for _, name := range []string{
			"admission.wait_queue_length.cpu",
			"admission.wait_queue_length.kv",
			"admission.wait_queue_length.sql-kv-response",
			"admission.wait_queue_length.sql-sql-response",
		} {
			ch.AdmissionQueueMax = max(ch.AdmissionQueueMax, int64(metricValue(n.Metrics, name)))
		}

		nh := model.CockroachNodeHealth{
			NodeID: n.NodeID, Status: status, Locality: n.Locality.String(), Version: n.BuildTag,
			StartedAt: unixNano(n.StartedAt), UpdatedAt: unixNano(n.UpdatedAt), CPUPercent: round2(cpu),
			RSSBytes: rss, MemoryBytes: n.TotalSystemMemory, MemoryUsedRatio: round4(memoryRatio), SQLConnections: conns,
		}
		ch.Nodes = append(ch.Nodes, nh)

		for storeID, metrics := range n.StoreMetrics {
			id, _ := strconv.Atoi(storeID)
			capacity := int64(metricValue(metrics, "capacity"))
			available := int64(metricValue(metrics, "capacity.available"))
			usedRatio := safeRatio(float64(capacity-available), float64(capacity))
			st := model.CockroachStoreHealth{
				NodeID: n.NodeID, StoreID: id, CapacityBytes: capacity, AvailableBytes: available,
				UsedRatio: round4(usedRatio), RangeReplicas: int64(metricValue(metrics, "replicas")),
				Leaseholders:          int64(metricValue(metrics, "replicas.leaseholders")),
				UnavailableRanges:     int64(metricValue(metrics, "ranges.unavailable")),
				UnderreplicatedRanges: int64(metricValue(metrics, "ranges.underreplicated")),
			}
			ch.Stores = append(ch.Stores, st)
			ch.StoresTotal++
			ch.CapacityBytes += capacity
			ch.AvailableBytes += available
			ch.RangeReplicas += st.RangeReplicas
			ch.UnavailableRanges += st.UnavailableRanges
			ch.UnderreplicatedRanges += st.UnderreplicatedRanges
			ch.MaxStoreUsedRatio = max(ch.MaxStoreUsedRatio, usedRatio)
		}
	}
	sort.Slice(ch.Nodes, func(i, j int) bool { return ch.Nodes[i].NodeID < ch.Nodes[j].NodeID })
	sort.Slice(ch.Stores, func(i, j int) bool {
		if ch.Stores[i].NodeID != ch.Stores[j].NodeID {
			return ch.Stores[i].NodeID < ch.Stores[j].NodeID
		}
		return ch.Stores[i].StoreID < ch.Stores[j].StoreID
	})
}

func assembleCockroachStorage(ch *model.CockroachHealth, before, after []crdbhttp.Node, dt time.Duration) {
	s := model.CockroachStorage{}
	if len(ch.Stores) == 0 {
		s.Section = unavail(nil, "CockroachDB storage and replication metrics require store metrics")
		ch.Storage = s
		return
	}

	previous := map[int]map[string]float64{}
	for _, n := range before {
		for rawID, metrics := range n.StoreMetrics {
			storeID, err := strconv.Atoi(rawID)
			if err == nil {
				previous[storeID] = metrics
			}
		}
	}

	mvccStores, replicationStores, replicaByteStores := 0, 0, 0
	for _, n := range after {
		status := cockroachLivenessStatus(n.LivenessStatus)
		for rawID, metrics := range n.StoreMetrics {
			storeID, err := strconv.Atoi(rawID)
			if err != nil {
				continue
			}
			capacity := int64(metricValue(metrics, "capacity"))
			available := int64(metricValue(metrics, "capacity.available"))
			filesystemUsed := max(int64(0), capacity-available)
			cockroachUsed := int64(metricValue(metrics, "capacity.used"))
			otherUsed := max(int64(0), filesystemUsed-cockroachUsed)
			liveBytes := int64(metricValue(metrics, "livebytes"))
			totalBytes := int64(metricValue(metrics, "totalbytes"))
			garbageBytes := max(int64(0), totalBytes-liveBytes)
			replicas := int64(metricValue(metrics, "replicas"))
			bytesPerReplica := safeRatio(float64(totalBytes), float64(replicas))

			row := model.CockroachStoreStorage{
				NodeID: n.NodeID, StoreID: storeID, Status: status, Locality: n.Locality.String(),
				CapacityBytes: capacity, FilesystemUsedBytes: filesystemUsed, CockroachUsedBytes: cockroachUsed,
				OtherUsedBytes: otherUsed, OtherUsedRatio: round4(safeRatio(float64(otherUsed), float64(capacity))),
				MVCCLiveBytes: liveBytes, MVCCTotalBytes: totalBytes, MVCCGarbageBytes: garbageBytes,
				BytesPerReplica: bytesPerReplica, RangeReplicas: replicas,
				UninitializedReplicas:    int64(metricValue(metrics, "replicas.uninitialized")),
				ReservedReplicas:         int64(metricValue(metrics, "replicas.reserved")),
				OverreplicatedRanges:     int64(metricValue(metrics, "ranges.overreplicated")),
				DecommissioningRanges:    int64(metricValue(metrics, "ranges.decommissioning")),
				RaftCommandsPending:      int64(metricValue(metrics, "raft.commands.pending")),
				RaftProbeFlows:           int64(metricValue(metrics, "raft.flows.state_probe")),
				RaftSnapshotFlows:        int64(metricValue(metrics, "raft.flows.state_snapshot")),
				ReplicateQueuePending:    int64(metricValue(metrics, "queue.replicate.pending")),
				ReplicateQueuePurgatory:  int64(metricValue(metrics, "queue.replicate.purgatory")),
				RaftSnapshotQueuePending: int64(metricValue(metrics, "queue.raftsnapshot.pending")),
			}

			if prior, ok := previous[storeID]; ok && dt > 0 {
				sampled := false
				if delta, ok := cockroachCounterDelta(prior, metrics, "storage.disk-slow"); ok {
					row.DiskSlowEvents = int64(delta)
					sampled = true
				}
				if delta, ok := cockroachCounterDelta(prior, metrics, "storage.disk-stalled"); ok {
					row.DiskStalledEvents = int64(delta)
					sampled = true
				}
				if delta, ok := cockroachCounterDelta(prior, metrics, "storage.disk-unhealthy.duration"); ok {
					row.DiskUnhealthySeconds = delta / float64(time.Second)
					sampled = true
				}
				if delta, ok := cockroachCounterDelta(prior, metrics, "storage.write-stalls"); ok {
					row.WriteStallEvents = int64(delta)
					sampled = true
				}
				if delta, ok := cockroachCounterDelta(prior, metrics, "storage.write-stall-nanos"); ok {
					row.WriteStallSeconds = delta / float64(time.Second)
					sampled = true
				}
				if delta, ok := cockroachCounterDelta(prior, metrics, "raft.dropped"); ok {
					row.RaftDroppedMessages = int64(delta)
					sampled = true
				}
				if sampled && status == "live" {
					s.CounterSampledStores++
				}
			}
			row.DiskUnhealthySeconds = round2(row.DiskUnhealthySeconds)
			row.WriteStallSeconds = round2(row.WriteStallSeconds)
			s.Stores = append(s.Stores, row)

			if status != "live" {
				continue
			}
			s.LiveStores++
			s.FilesystemUsedBytes += filesystemUsed
			s.CockroachUsedBytes += cockroachUsed
			s.OtherUsedBytes += otherUsed
			if row.OtherUsedRatio > s.MaxOtherUsedRatio {
				s.MaxOtherUsedRatio = row.OtherUsedRatio
				s.MaxOtherUsedStoreID = storeID
			}
			if _, liveOK := metrics["livebytes"]; liveOK {
				if _, totalOK := metrics["totalbytes"]; totalOK {
					mvccStores++
					s.MVCCLiveBytes += liveBytes
					s.MVCCTotalBytes += totalBytes
					s.MVCCGarbageBytes += garbageBytes
				}
			}
			if _, ok := metrics["replicas.uninitialized"]; ok {
				replicationStores++
			}
			if bytesPerReplica > 0 {
				replicaByteStores++
				s.BytesPerReplicaMean += bytesPerReplica
				if s.SmallestReplicaBytesStoreID == 0 || bytesPerReplica < s.BytesPerReplicaMin {
					s.BytesPerReplicaMin = bytesPerReplica
					s.SmallestReplicaBytesStoreID = storeID
				}
				if s.LargestReplicaBytesStoreID == 0 || bytesPerReplica > s.BytesPerReplicaMax {
					s.BytesPerReplicaMax = bytesPerReplica
					s.LargestReplicaBytesStoreID = storeID
				}
			}
			s.RangeReplicas += replicas
			s.UninitializedReplicas += row.UninitializedReplicas
			s.ReservedReplicas += row.ReservedReplicas
			s.OverreplicatedRanges += row.OverreplicatedRanges
			s.DecommissioningRanges += row.DecommissioningRanges
			s.RaftCommandsPending += row.RaftCommandsPending
			if row.RaftCommandsPending > s.MaxRaftCommandsPending {
				s.MaxRaftCommandsPending = row.RaftCommandsPending
				s.MaxRaftPendingStoreID = storeID
			}
			s.RaftProbeFlows += row.RaftProbeFlows
			s.RaftSnapshotFlows += row.RaftSnapshotFlows
			s.ReplicateQueuePending += row.ReplicateQueuePending
			s.ReplicateQueuePurgatory += row.ReplicateQueuePurgatory
			s.RaftSnapshotQueuePending += row.RaftSnapshotQueuePending
			s.DiskSlowEvents += row.DiskSlowEvents
			s.DiskStalledEvents += row.DiskStalledEvents
			s.DiskUnhealthySeconds += row.DiskUnhealthySeconds
			s.WriteStallEvents += row.WriteStallEvents
			s.WriteStallSeconds += row.WriteStallSeconds
			s.RaftDroppedMessages += row.RaftDroppedMessages
		}
	}

	s.MVCCMetricsAvailable = mvccStores > 0
	s.ReplicationMetricsAvailable = replicationStores > 0
	if s.MVCCTotalBytes > 0 {
		s.MVCCLiveRatio = safeRatio(float64(s.MVCCLiveBytes), float64(s.MVCCTotalBytes))
	}
	if replicaByteStores > 0 {
		s.BytesPerReplicaMean /= float64(replicaByteStores)
	}
	s.SampleSeconds = round2(dt.Seconds())
	s.MaxOtherUsedRatio = round4(s.MaxOtherUsedRatio)
	s.MVCCLiveRatio = round4(s.MVCCLiveRatio)
	s.BytesPerReplicaMin = round2(s.BytesPerReplicaMin)
	s.BytesPerReplicaMean = round2(s.BytesPerReplicaMean)
	s.BytesPerReplicaMax = round2(s.BytesPerReplicaMax)
	s.DiskUnhealthySeconds = round2(s.DiskUnhealthySeconds)
	s.WriteStallSeconds = round2(s.WriteStallSeconds)
	s.Section = model.Section{Exactness: model.ExactnessScraped}
	if s.CounterSampledStores > 0 {
		s.Exactness = model.ExactnessSampled
	}
	var partial []string
	if mvccStores < s.LiveStores {
		partial = append(partial, fmtCoverage("MVCC", mvccStores, s.LiveStores))
	}
	if replicationStores < s.LiveStores {
		partial = append(partial, fmtCoverage("replication", replicationStores, s.LiveStores))
	}
	if s.CounterSampledStores < s.LiveStores {
		partial = append(partial, fmtCoverage("counter sample", s.CounterSampledStores, s.LiveStores))
	}
	if len(partial) > 0 {
		s.Reason = strings.Join(partial, "; ")
	}
	sort.Slice(s.Stores, func(i, j int) bool { return s.Stores[i].StoreID < s.Stores[j].StoreID })
	ch.Storage = s
}

func cockroachCounterDelta(before, after map[string]float64, name string) (float64, bool) {
	a, aok := before[name]
	b, bok := after[name]
	if !aok || !bok || b < a {
		return 0, false
	}
	return b - a, true
}

func fmtCoverage(name string, have, total int) string {
	return name + " metrics on " + strconv.Itoa(have) + "/" + strconv.Itoa(total) + " live stores"
}

func assembleCockroachDistribution(h *model.CockroachHealth) {
	d := model.CockroachDistribution{}
	if len(h.Stores) == 0 {
		d.Section = unavail(nil, "CockroachDB store distribution requires Admin API node metrics")
		h.Distribution = d
		return
	}
	nodes := make(map[int]model.CockroachNodeHealth, len(h.Nodes))
	for _, n := range h.Nodes {
		nodes[n.NodeID] = n
	}
	type hotSummary struct {
		count int
		cpu   float64
	}
	hotByStore := map[int]hotSummary{}
	hotByLeaseholder := map[int]hotSummary{}
	for _, r := range h.Hot {
		d.HotRangeSampleCount++
		d.HotRangeCPUCores += r.CPUCores
		if r.StoreID > 0 {
			x := hotByStore[r.StoreID]
			x.count++
			x.cpu += r.CPUCores
			hotByStore[r.StoreID] = x
		}
		if r.LeaseholderNodeID > 0 {
			d.HotRangeLeaseholderSamples++
			x := hotByLeaseholder[r.LeaseholderNodeID]
			x.count++
			x.cpu += r.CPUCores
			hotByLeaseholder[r.LeaseholderNodeID] = x
		}
	}
	for nodeID, x := range hotByLeaseholder {
		if x.cpu > d.HottestLeaseholderCPUCores || (x.cpu == d.HottestLeaseholderCPUCores && nodeID < d.HottestLeaseholderNodeID) {
			d.HottestLeaseholderNodeID = nodeID
			d.HottestLeaseholderRanges = x.count
			d.HottestLeaseholderCPUCores = x.cpu
		}
	}
	if d.HotRangeCPUCores > 0 {
		d.HottestLeaseholderCPUShare = d.HottestLeaseholderCPUCores / d.HotRangeCPUCores
	}

	var liveCapacities []int64
	for _, s := range h.Stores {
		n, ok := nodes[s.NodeID]
		if !ok || n.Status != "live" {
			d.ExcludedStores++
			continue
		}
		d.LiveStores++
		if s.CapacityBytes > 0 {
			liveCapacities = append(liveCapacities, s.CapacityBytes)
		}
	}
	sort.Slice(liveCapacities, func(i, j int) bool { return liveCapacities[i] < liveCapacities[j] })
	medianCapacity := medianInt64(liveCapacities)
	localities := map[string]bool{}
	for _, s := range h.Stores {
		n, ok := nodes[s.NodeID]
		live := ok && n.Status == "live"
		comparable := live && (medianCapacity == 0 || (s.CapacityBytes > 0 && float64(s.CapacityBytes) >= float64(medianCapacity)*.75 && float64(s.CapacityBytes) <= float64(medianCapacity)*1.25))
		hot := hotByStore[s.StoreID]
		row := model.CockroachStoreBalance{
			NodeID: s.NodeID, StoreID: s.StoreID, Status: n.Status, Locality: n.Locality, Comparable: comparable,
			CapacityBytes: s.CapacityBytes, UsedRatio: s.UsedRatio, RangeReplicas: s.RangeReplicas,
			Leaseholders: s.Leaseholders, NodeCPUPercent: n.CPUPercent, TopHotRanges: hot.count, TopHotCPUCores: round4(hot.cpu),
		}
		d.Stores = append(d.Stores, row)
		if !live {
			continue
		}
		if n.Locality != "" {
			localities[n.Locality] = true
		}
		if d.LeastUsedStoreID == 0 || s.UsedRatio < d.CapacityUsedMinRatio {
			d.CapacityUsedMinRatio = s.UsedRatio
			d.LeastUsedStoreID = s.StoreID
		}
		if d.MostUsedStoreID == 0 || s.UsedRatio > d.CapacityUsedMaxRatio {
			d.CapacityUsedMaxRatio = s.UsedRatio
			d.MostUsedStoreID = s.StoreID
		}
		if comparable {
			d.ComparableStores++
			if d.FewestReplicasStoreID == 0 || s.RangeReplicas < d.ReplicaMin {
				d.ReplicaMin = s.RangeReplicas
				d.FewestReplicasStoreID = s.StoreID
			}
			if d.MostReplicasStoreID == 0 || s.RangeReplicas > d.ReplicaMax {
				d.ReplicaMax = s.RangeReplicas
				d.MostReplicasStoreID = s.StoreID
			}
			d.ReplicaMean += float64(s.RangeReplicas)
			if d.FewestLeasesStoreID == 0 || s.Leaseholders < d.LeaseMin {
				d.LeaseMin = s.Leaseholders
				d.FewestLeasesStoreID = s.StoreID
			}
			if d.MostLeasesStoreID == 0 || s.Leaseholders > d.LeaseMax {
				d.LeaseMax = s.Leaseholders
				d.MostLeasesStoreID = s.StoreID
			}
			d.LeaseMean += float64(s.Leaseholders)
		}
	}
	d.MultipleLocalities = len(localities) > 1
	if d.ComparableStores > 0 {
		d.ReplicaMean /= float64(d.ComparableStores)
		d.LeaseMean /= float64(d.ComparableStores)
		if d.ReplicaMean > 0 {
			d.ReplicaMinToMean = float64(d.ReplicaMin) / d.ReplicaMean
			d.ReplicaMaxToMean = float64(d.ReplicaMax) / d.ReplicaMean
		}
		if d.LeaseMean > 0 {
			d.LeaseMinToMean = float64(d.LeaseMin) / d.LeaseMean
			d.LeaseMaxToMean = float64(d.LeaseMax) / d.LeaseMean
		}
	}
	d.CapacityUsedSpread = d.CapacityUsedMaxRatio - d.CapacityUsedMinRatio
	d.ReplicaMean = round2(d.ReplicaMean)
	d.ReplicaMinToMean = round4(d.ReplicaMinToMean)
	d.ReplicaMaxToMean = round4(d.ReplicaMaxToMean)
	d.LeaseMean = round2(d.LeaseMean)
	d.LeaseMinToMean = round4(d.LeaseMinToMean)
	d.LeaseMaxToMean = round4(d.LeaseMaxToMean)
	d.CapacityUsedMinRatio = round4(d.CapacityUsedMinRatio)
	d.CapacityUsedMaxRatio = round4(d.CapacityUsedMaxRatio)
	d.CapacityUsedSpread = round4(d.CapacityUsedSpread)
	d.HotRangeCPUCores = round4(d.HotRangeCPUCores)
	d.HottestLeaseholderCPUCores = round4(d.HottestLeaseholderCPUCores)
	d.HottestLeaseholderCPUShare = round4(d.HottestLeaseholderCPUShare)
	d.Section = model.Section{Exactness: model.ExactnessScraped}
	if h.HotRanges.Exactness == model.ExactnessUnavailable {
		d.Reason = "top-hot-range concentration unavailable: " + h.HotRanges.Reason
	} else if h.HotRanges.Reason != "" {
		d.Reason = "top-hot-range concentration is partial: " + h.HotRanges.Reason
	}
	h.Distribution = d
}

func medianInt64(values []int64) int64 {
	if len(values) == 0 {
		return 0
	}
	mid := len(values) / 2
	if len(values)%2 == 1 {
		return values[mid]
	}
	return values[mid-1]/2 + values[mid]/2
}

func cockroachLoadRates(a, b []crdbhttp.Load, dt time.Duration) (queries, newConnections float64, sampled int) {
	if dt <= 0 {
		return 0, 0, 0
	}
	byNode := make(map[int]crdbhttp.Load, len(a))
	for _, l := range a {
		byNode[l.NodeID] = l
	}
	for _, end := range b {
		start, ok := byNode[end.NodeID]
		if !ok || end.QueryCount < start.QueryCount || end.NewConnections < start.NewConnections {
			continue
		}
		queries += (end.QueryCount - start.QueryCount) / dt.Seconds()
		newConnections += (end.NewConnections - start.NewConnections) / dt.Seconds()
		sampled++
	}
	return round2(queries), round2(newConnections), sampled
}

func cockroachLivenessStatus(v int) string {
	switch v {
	case 1:
		return "dead"
	case 2:
		return "unavailable"
	case 3:
		return "live"
	case 4:
		return "decommissioning"
	case 5:
		return "decommissioned"
	case 6:
		return "draining"
	default:
		return "unknown"
	}
}

func nodeCPUPercent(metrics map[string]float64) float64 {
	v := metricValue(metrics, "sys.cpu.combined.percent-normalized")
	if v <= 1.5 {
		return v * 100
	}
	return v
}

func metricValue(metrics map[string]float64, name string) float64 {
	if metrics == nil {
		return 0
	}
	return metrics[name]
}

func safeRatio(n, d float64) float64 {
	if d <= 0 || n <= 0 {
		return 0
	}
	return n / d
}

func unixNano(v int64) time.Time {
	if v <= 0 {
		return time.Time{}
	}
	return time.Unix(0, v).UTC()
}
