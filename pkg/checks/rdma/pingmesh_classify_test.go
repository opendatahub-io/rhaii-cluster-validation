package rdma

import (
	"bytes"
	"testing"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/jobrunner"
)

func makeTopo(devs ...string) *checks.NodeTopology {
	var pairs []checks.GPUNICPair
	for i, d := range devs {
		pairs = append(pairs, checks.GPUNICPair{
			GPU: checks.GPUInfo{ID: i},
			NIC: checks.NICInfo{Dev: d},
		})
	}
	return &checks.NodeTopology{Pairs: pairs}
}

func TestClassifyPingMeshResults(t *testing.T) {
	topoMap := map[string]*checks.NodeTopology{
		"nodeA": makeTopo("ibp0", "ibp1"),
		"nodeB": makeTopo("ibp0", "ibp1"),
	}

	t.Run("all pass rail and xrail", func(t *testing.T) {
		pair := jobrunner.NodePair{Server: "nodeA", Client: "nodeB"}
		results := map[jobrunner.NodePair][]jobrunner.JobResult{
			pair: {
				{
					Status: checks.StatusPass,
					Details: []PingMeshPairResult{
						{SrcDev: "ibp0", DstDev: "ibp0", Pass: true}, // rail (0==0)
						{SrcDev: "ibp1", DstDev: "ibp1", Pass: true}, // rail (1==1)
						{SrcDev: "ibp0", DstDev: "ibp1", Pass: true}, // xrail (0!=1)
						{SrcDev: "ibp1", DstDev: "ibp0", Pass: true}, // xrail (1!=0)
					},
				},
			},
		}
		report, failures := ClassifyPingMeshResults(results, topoMap, &bytes.Buffer{})
		if report == nil {
			t.Fatal("nil report")
		}

		rail := report.Summary["rdma_conn_rail"]
		if rail.Status != checks.StatusPass {
			t.Errorf("rail status = %q, want PASS", rail.Status)
		}
		if rail.Passed != 2 || rail.Total != 2 {
			t.Errorf("rail = %d/%d, want 2/2", rail.Passed, rail.Total)
		}

		xrail := report.Summary["rdma_conn_xrail"]
		if xrail.Status != checks.StatusPass {
			t.Errorf("xrail status = %q, want PASS", xrail.Status)
		}
		if xrail.Passed != 2 || xrail.Total != 2 {
			t.Errorf("xrail = %d/%d, want 2/2", xrail.Passed, xrail.Total)
		}

		if len(failures.Failures) != 0 {
			t.Errorf("expected no failures, got %d", len(failures.Failures))
		}
	})

	t.Run("rail pass xrail fail", func(t *testing.T) {
		pair := jobrunner.NodePair{Server: "nodeA", Client: "nodeB"}
		results := map[jobrunner.NodePair][]jobrunner.JobResult{
			pair: {
				{
					Status: checks.StatusFail,
					Details: []PingMeshPairResult{
						{SrcDev: "ibp0", DstDev: "ibp0", Pass: true},
						{SrcDev: "ibp1", DstDev: "ibp1", Pass: true},
						{SrcDev: "ibp0", DstDev: "ibp1", Pass: false, Error: "timeout"},
						{SrcDev: "ibp1", DstDev: "ibp0", Pass: false, Error: "timeout"},
					},
				},
			},
		}
		report, failures := ClassifyPingMeshResults(results, topoMap, &bytes.Buffer{})

		rail := report.Summary["rdma_conn_rail"]
		if rail.Status != checks.StatusPass {
			t.Errorf("rail status = %q, want PASS", rail.Status)
		}

		xrail := report.Summary["rdma_conn_xrail"]
		if xrail.Status != checks.StatusFail {
			t.Errorf("xrail status = %q, want FAIL", xrail.Status)
		}
		if xrail.Passed != 0 || xrail.Total != 2 {
			t.Errorf("xrail = %d/%d, want 0/2", xrail.Passed, xrail.Total)
		}

		if len(failures.Failures) != 2 {
			t.Errorf("expected 2 failures, got %d", len(failures.Failures))
		}
	})

	t.Run("retry succeeds on second attempt", func(t *testing.T) {
		pair := jobrunner.NodePair{Server: "nodeA", Client: "nodeB"}
		results := map[jobrunner.NodePair][]jobrunner.JobResult{
			pair: {
				{
					Status: checks.StatusFail,
					Details: []PingMeshPairResult{
						{SrcDev: "ibp0", DstDev: "ibp0", Pass: false, Error: "timeout"},
					},
				},
				{
					Status: checks.StatusPass,
					Details: []PingMeshPairResult{
						{SrcDev: "ibp0", DstDev: "ibp0", Pass: true},
					},
				},
			},
		}
		report, failures := ClassifyPingMeshResults(results, topoMap, &bytes.Buffer{})

		rail := report.Summary["rdma_conn_rail"]
		if rail.Status != checks.StatusPass {
			t.Errorf("rail status = %q, want PASS (should succeed on retry)", rail.Status)
		}
		if rail.Passed != 1 || rail.Total != 1 {
			t.Errorf("rail = %d/%d, want 1/1", rail.Passed, rail.Total)
		}

		if len(failures.Failures) != 0 {
			t.Errorf("expected no failures (retried ok), got %d", len(failures.Failures))
		}
	})

	t.Run("missing topology skips pair", func(t *testing.T) {
		pair := jobrunner.NodePair{Server: "nodeA", Client: "unknown"}
		results := map[jobrunner.NodePair][]jobrunner.JobResult{
			pair: {
				{
					Status: checks.StatusPass,
					Details: []PingMeshPairResult{
						{SrcDev: "ibp0", DstDev: "ibp0", Pass: true},
					},
				},
			},
		}
		report, _ := ClassifyPingMeshResults(results, topoMap, &bytes.Buffer{})

		rail := report.Summary["rdma_conn_rail"]
		xrail := report.Summary["rdma_conn_xrail"]
		if rail.Total != 0 || xrail.Total != 0 {
			t.Errorf("expected 0 total pairs with missing topology, got rail=%d xrail=%d", rail.Total, xrail.Total)
		}
	})

	t.Run("single NIC per node has no xrail", func(t *testing.T) {
		singleTopoMap := map[string]*checks.NodeTopology{
			"a": makeTopo("ibp0"),
			"b": makeTopo("ibp0"),
		}
		pair := jobrunner.NodePair{Server: "a", Client: "b"}
		results := map[jobrunner.NodePair][]jobrunner.JobResult{
			pair: {
				{
					Status: checks.StatusPass,
					Details: []PingMeshPairResult{
						{SrcDev: "ibp0", DstDev: "ibp0", Pass: true},
					},
				},
			},
		}
		report, _ := ClassifyPingMeshResults(results, singleTopoMap, &bytes.Buffer{})

		rail := report.Summary["rdma_conn_rail"]
		if rail.Total != 1 || rail.Passed != 1 {
			t.Errorf("rail = %d/%d, want 1/1", rail.Passed, rail.Total)
		}

		xrail := report.Summary["rdma_conn_xrail"]
		if xrail.Total != 0 {
			t.Errorf("xrail total = %d, want 0 (single NIC)", xrail.Total)
		}
		if xrail.Status != checks.StatusSkip {
			t.Errorf("xrail status = %q, want SKIP", xrail.Status)
		}
	})
}

func TestPingMeshStatus(t *testing.T) {
	tests := []struct {
		passed, total int
		want          checks.Status
	}{
		{0, 0, checks.StatusSkip},
		{8, 8, checks.StatusPass},
		{4, 8, checks.StatusWarn},
		{0, 8, checks.StatusFail},
	}
	for _, tt := range tests {
		got := PingMeshStatus(tt.passed, tt.total)
		if got != tt.want {
			t.Errorf("PingMeshStatus(%d, %d) = %q, want %q", tt.passed, tt.total, got, tt.want)
		}
	}
}

func TestBuildRailMap(t *testing.T) {
	topo := makeTopo("ibp0", "ibp1", "ibp2")
	m := BuildRailMap(topo)
	if m["ibp0"] != 0 || m["ibp1"] != 1 || m["ibp2"] != 2 {
		t.Errorf("unexpected rail map: %v", m)
	}

	nilMap := BuildRailMap(nil)
	if len(nilMap) != 0 {
		t.Errorf("BuildRailMap(nil) should return empty map, got %v", nilMap)
	}
}
