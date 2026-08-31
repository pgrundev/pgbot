package collect

import (
	"testing"
	"time"

	"github.com/pgrundev/pgbot/internal/crdbhttp"
	"github.com/pgrundev/pgbot/internal/model"
)

func TestAssembleCockroachHealth(t *testing.T) {
	a := cockroachHealthSample{Loads: []crdbhttp.Load{{NodeID: 1, QueryCount: 100, NewConnections: 10}}}
	b := cockroachHealthSample{
		Loads: []crdbhttp.Load{{NodeID: 1, QueryCount: 140, NewConnections: 14, SQLConnections: 7}},
		Nodes: []crdbhttp.Node{{
			NodeID: 1, BuildTag: "v25.2.1", LivenessStatus: 3, TotalSystemMemory: 1000,
			Metrics:      map[string]float64{"sys.cpu.combined.percent-normalized": .72, "sys.rss": 500, "sql.conns": 7, "sql.service.latency-p99": 12e6},
			StoreMetrics: map[string]map[string]float64{"4": {"capacity": 100, "capacity.available": 35, "replicas": 20, "ranges": 7, "replicas.leaseholders": 7, "ranges.unavailable": 1, "ranges.underreplicated": 2}},
		}},
	}
	c := &model.Context{Server: model.ServerInfo{Engine: "cockroachdb"}}
	assembleCockroachHealth(c, sampled{A: a, B: b}, 2*time.Second)
	h := c.Health.Cockroach
	if h.NodesLive != 1 || h.StoresTotal != 1 || h.UnavailableRanges != 1 || h.MaxStoreUsedRatio != .65 {
		t.Fatalf("health=%+v", h)
	}
	if h.RangeReplicas != 20 || h.Stores[0].RangeReplicas != 20 || h.Stores[0].Leaseholders != 7 {
		t.Fatalf("store replica metrics=%+v", h.Stores[0])
	}
	if h.QueriesPerSec == nil || *h.QueriesPerSec != 20 || h.NewConnectionsPerSec == nil || *h.NewConnectionsPerSec != 2 {
		t.Fatalf("rates=%v/%v", h.QueriesPerSec, h.NewConnectionsPerSec)
	}
	if h.MaxCPUPercent != 72 || h.ServiceLatencyP99MS != 12 {
		t.Fatalf("node metrics=%+v", h)
	}
}

func TestHottestRangesDeduplicatesReplicas(t *testing.T) {
	got := hottestRanges([]crdbhttp.HotRange{
		{RangeID: 1, NodeID: 1, CPUTimePerSecond: 100e6},
		{RangeID: 1, NodeID: 2, CPUTimePerSecond: 400e6},
		{RangeID: 2, NodeID: 1, CPUTimePerSecond: 200e6},
	}, 10)
	if len(got) != 2 || got[0].RangeID != 1 || got[0].NodeID != 2 || got[0].CPUCores != .4 {
		t.Fatalf("hottest=%+v", got)
	}
}

func TestAssembleCockroachDistribution(t *testing.T) {
	h := &model.CockroachHealth{
		HotRanges: model.Section{Exactness: model.ExactnessScraped},
		Nodes: []model.CockroachNodeHealth{
			{NodeID: 1, Status: "live", Locality: "region=east,zone=a", CPUPercent: 82},
			{NodeID: 2, Status: "live", Locality: "region=east,zone=b", CPUPercent: 35},
			{NodeID: 3, Status: "live", Locality: "region=west,zone=c", CPUPercent: 28},
			{NodeID: 4, Status: "draining", Locality: "region=west,zone=d", CPUPercent: 10},
		},
		Stores: []model.CockroachStoreHealth{
			{NodeID: 1, StoreID: 1, CapacityBytes: 100, UsedRatio: .80, RangeReplicas: 300, Leaseholders: 220},
			{NodeID: 2, StoreID: 2, CapacityBytes: 100, UsedRatio: .40, RangeReplicas: 100, Leaseholders: 40},
			{NodeID: 3, StoreID: 3, CapacityBytes: 100, UsedRatio: .30, RangeReplicas: 100, Leaseholders: 40},
			{NodeID: 4, StoreID: 4, CapacityBytes: 100, UsedRatio: .90, RangeReplicas: 500, Leaseholders: 400},
		},
	}
	for i := 0; i < 5; i++ {
		nodeID, storeID, cpu := 1, 1, .2
		if i == 4 {
			nodeID, storeID, cpu = 2, 2, .1
		}
		h.Hot = append(h.Hot, model.CockroachHotRange{RangeID: int64(i + 1), NodeID: nodeID, StoreID: storeID, LeaseholderNodeID: nodeID, CPUCores: cpu})
	}
	assembleCockroachDistribution(h)
	d := &h.Distribution
	if d.Exactness != model.ExactnessScraped || d.LiveStores != 3 || d.ComparableStores != 3 || d.ExcludedStores != 1 {
		t.Fatalf("coverage=%+v", d)
	}
	if d.ReplicaMin != 100 || d.ReplicaMax != 300 || d.ReplicaMean != 166.67 || d.MostReplicasStoreID != 1 {
		t.Fatalf("replica balance=%+v", d)
	}
	if d.LeaseMin != 40 || d.LeaseMax != 220 || d.LeaseMean != 100 || d.MostLeasesStoreID != 1 {
		t.Fatalf("lease balance=%+v", d)
	}
	if d.CapacityUsedMinRatio != .3 || d.CapacityUsedMaxRatio != .8 || d.CapacityUsedSpread != .5 {
		t.Fatalf("capacity balance=%+v", d)
	}
	if d.HottestLeaseholderNodeID != 1 || d.HottestLeaseholderRanges != 4 || d.HottestLeaseholderCPUShare != .8889 || !d.MultipleLocalities {
		t.Fatalf("hot concentration=%+v", d)
	}
}

func TestAssembleCockroachDistributionExcludesDifferentSizedStoreFromReplicaComparison(t *testing.T) {
	h := &model.CockroachHealth{
		HotRanges: model.Section{Exactness: model.ExactnessUnavailable, Reason: "not configured"},
		Nodes:     []model.CockroachNodeHealth{{NodeID: 1, Status: "live"}, {NodeID: 2, Status: "live"}, {NodeID: 3, Status: "live"}},
		Stores: []model.CockroachStoreHealth{
			{NodeID: 1, StoreID: 1, CapacityBytes: 100, RangeReplicas: 100},
			{NodeID: 2, StoreID: 2, CapacityBytes: 100, RangeReplicas: 110},
			{NodeID: 3, StoreID: 3, CapacityBytes: 300, RangeReplicas: 500},
		},
	}
	assembleCockroachDistribution(h)
	if h.Distribution.ComparableStores != 2 || h.Distribution.ReplicaMin != 100 || h.Distribution.ReplicaMax != 110 {
		t.Fatalf("heterogeneous capacity comparison=%+v", h.Distribution)
	}
	if h.Distribution.Reason == "" {
		t.Fatal("missing hot-range source must make distribution partial")
	}
}

func TestAssembleCockroachStorage(t *testing.T) {
	before := []crdbhttp.Node{{
		NodeID: 1, LivenessStatus: 3,
		StoreMetrics: map[string]map[string]float64{
			"1": {
				"capacity": 1000, "capacity.available": 100, "capacity.used": 600,
				"livebytes": 400, "totalbytes": 500, "replicas": 10,
				"replicas.uninitialized": 2, "replicas.reserved": 1,
				"ranges.overreplicated": 3, "ranges.decommissioning": 4,
				"raft.commands.pending": 5, "raft.flows.state_probe": 6, "raft.flows.state_snapshot": 7,
				"queue.replicate.pending": 8, "queue.replicate.purgatory": 9, "queue.raftsnapshot.pending": 10,
				"storage.disk-slow": 1, "storage.disk-stalled": 0, "storage.disk-unhealthy.duration": 10e9,
				"storage.write-stalls": 2, "storage.write-stall-nanos": 1e9, "raft.dropped": 10,
			},
			"2": {
				"capacity": 1000, "capacity.available": 400, "capacity.used": 550,
				"livebytes": 300, "totalbytes": 400, "replicas": 20,
				"replicas.uninitialized": 1, "replicas.reserved": 2,
				"ranges.overreplicated": 0, "ranges.decommissioning": 0,
				"raft.commands.pending": 3, "raft.flows.state_probe": 0, "raft.flows.state_snapshot": 1,
				"queue.replicate.pending": 4, "queue.replicate.purgatory": 0, "queue.raftsnapshot.pending": 0,
				"storage.disk-slow": 0, "storage.disk-stalled": 0, "storage.disk-unhealthy.duration": 0,
				"storage.write-stalls": 0, "storage.write-stall-nanos": 0, "raft.dropped": 2,
			},
		},
	}}
	after := before
	after = append([]crdbhttp.Node(nil), before...)
	after[0].StoreMetrics = map[string]map[string]float64{}
	for id, metrics := range before[0].StoreMetrics {
		copyMetrics := map[string]float64{}
		for name, value := range metrics {
			copyMetrics[name] = value
		}
		after[0].StoreMetrics[id] = copyMetrics
	}
	after[0].StoreMetrics["1"]["storage.disk-slow"] = 3
	after[0].StoreMetrics["1"]["storage.disk-unhealthy.duration"] = 12e9
	after[0].StoreMetrics["1"]["storage.write-stalls"] = 3
	after[0].StoreMetrics["1"]["storage.write-stall-nanos"] = 1.5e9
	after[0].StoreMetrics["1"]["raft.dropped"] = 15

	h := &model.CockroachHealth{}
	assembleCockroachNodes(h, after)
	assembleCockroachStorage(h, before, after, 2*time.Second)
	s := &h.Storage
	if s.Exactness != model.ExactnessSampled || s.LiveStores != 2 || s.CounterSampledStores != 2 {
		t.Fatalf("coverage=%+v", s)
	}
	if s.FilesystemUsedBytes != 1500 || s.CockroachUsedBytes != 1150 || s.OtherUsedBytes != 350 || s.MaxOtherUsedStoreID != 1 || s.MaxOtherUsedRatio != .3 {
		t.Fatalf("capacity ownership=%+v", s)
	}
	if s.MVCCLiveBytes != 700 || s.MVCCTotalBytes != 900 || s.MVCCGarbageBytes != 200 || s.MVCCLiveRatio != .7778 {
		t.Fatalf("MVCC=%+v", s)
	}
	if s.BytesPerReplicaMin != 20 || s.BytesPerReplicaMean != 35 || s.BytesPerReplicaMax != 50 {
		t.Fatalf("bytes/replica=%+v", s)
	}
	if s.UninitializedReplicas != 3 || s.ReservedReplicas != 3 || s.OverreplicatedRanges != 3 || s.DecommissioningRanges != 4 {
		t.Fatalf("replication=%+v", s)
	}
	if s.RaftCommandsPending != 8 || s.RaftProbeFlows != 6 || s.RaftSnapshotFlows != 8 || s.ReplicateQueuePending != 12 || s.ReplicateQueuePurgatory != 9 || s.RaftSnapshotQueuePending != 10 {
		t.Fatalf("queues=%+v", s)
	}
	if s.DiskSlowEvents != 2 || s.DiskStalledEvents != 0 || s.DiskUnhealthySeconds != 2 || s.WriteStallEvents != 1 || s.WriteStallSeconds != .5 || s.RaftDroppedMessages != 5 {
		t.Fatalf("counter deltas=%+v", s)
	}
}
