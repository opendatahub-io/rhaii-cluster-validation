package controller

import (
	"bytes"
	"strings"
	"testing"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks/rdma"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/jobrunner"
)

func TestParseReport(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantNode string
		wantLen  int
		wantErr  bool
	}{
		{
			name: "stderr lines then JSON",
			input: `[PASS] gpu_hardware/gpu_driver_version: NVIDIA driver: 535.129.03
[PASS] gpu_hardware/gpu_ecc_status: No errors
[FAIL] networking_rdma/rdma_devices_detected: No RDMA devices
{
  "node": "gpu-node-1",
  "timestamp": "2024-01-01T00:00:00Z",
  "results": [
    {
      "category": "gpu_hardware",
      "name": "gpu_driver_version",
      "status": "PASS",
      "message": "OK"
    },
    {
      "category": "networking_rdma",
      "name": "rdma_devices_detected",
      "status": "FAIL",
      "message": "No RDMA devices"
    }
  ]
}`,
			wantNode: "gpu-node-1",
			wantLen:  2,
		},
		{
			name: "JSON only no stderr",
			input: `{
  "node": "node-2",
  "timestamp": "2024-01-01T00:00:00Z",
  "results": [
    {
      "category": "gpu_hardware",
      "name": "gpu_ecc_status",
      "status": "PASS",
      "message": "clean"
    }
  ]
}`,
			wantNode: "node-2",
			wantLen:  1,
		},
		{
			name:    "no JSON at all",
			input:   "some random log line\nanother line\n",
			wantErr: true,
		},
		{
			name:    "empty input",
			input:   "",
			wantErr: true,
		},
		{
			name: "truncated JSON",
			input: `[PASS] check: ok
{
  "node": "n1",
  "results": [`,
			wantErr: true,
		},
		{
			name: "JSON followed by stderr lines",
			input: `Platform config: AKS
[FAIL] gpu_hardware/gpu_driver_version: nvidia-smi failed: exit status 12
[SKIP] gpu_hardware/gpu_ecc_status: nvidia-smi ECC query failed: exit status 12
[PASS] networking_rdma/rdma_devices_detected: 1 RDMA device(s) found: mlx5_0
{
  "node": "aks-gpupool-vmss000015",
  "timestamp": "2026-03-12T18:21:55Z",
  "results": [
    {
      "category": "gpu_hardware",
      "name": "gpu_driver_version",
      "status": "FAIL",
      "message": "nvidia-smi failed: exit status 12"
    }
  ]
}
Validation failed: one or more checks reported FAIL
Waiting for controller to collect results...`,
			wantNode: "aks-gpupool-vmss000015",
			wantLen:  1,
		},
		{
			name: "JSON with single result",
			input: `Platform config: aks
{
  "node": "aks-gpu-0",
  "timestamp": "2024-06-15T12:00:00Z",
  "results": [
    {
      "category": "gpu_hardware",
      "name": "gpu_driver_version",
      "status": "PASS",
      "message": "NVIDIA driver: 535.129.03, CUDA: 12.2, GPU: NVIDIA A100-SXM4-80GB (81920 MiB), 8 GPU(s)",
      "details": {
        "driver_version": "535.129.03",
        "gpu_count": 8
      }
    }
  ]
}`,
			wantNode: "aks-gpu-0",
			wantLen:  1,
		},
		{
			name:    "interleaved stderr inside JSON is not recoverable",
			input: `{
  "node": "gpu-node-1",
  "results": [
    {
      "status": "PASS",
    Validation complete: all checks passed
      "message": "ok"
    }
  ]
}`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report, err := parseReport(strings.NewReader(tt.input))
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if report.Node != tt.wantNode {
				t.Errorf("Node = %q, want %q", report.Node, tt.wantNode)
			}
			if len(report.Results) != tt.wantLen {
				t.Errorf("got %d results, want %d", len(report.Results), tt.wantLen)
			}
		})
	}
}

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
	c := &Controller{output: &bytes.Buffer{}}
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
					Details: []rdma.PingMeshPairResult{
						{SrcDev: "ibp0", DstDev: "ibp0", Pass: true},  // rail (0==0)
						{SrcDev: "ibp1", DstDev: "ibp1", Pass: true},  // rail (1==1)
						{SrcDev: "ibp0", DstDev: "ibp1", Pass: true},  // xrail (0!=1)
						{SrcDev: "ibp1", DstDev: "ibp0", Pass: true},  // xrail (1!=0)
					},
				},
			},
		}
		report, failures := c.classifyPingMeshResults(results, topoMap)
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
					Details: []rdma.PingMeshPairResult{
						{SrcDev: "ibp0", DstDev: "ibp0", Pass: true},
						{SrcDev: "ibp1", DstDev: "ibp1", Pass: true},
						{SrcDev: "ibp0", DstDev: "ibp1", Pass: false, Error: "timeout"},
						{SrcDev: "ibp1", DstDev: "ibp0", Pass: false, Error: "timeout"},
					},
				},
			},
		}
		report, failures := c.classifyPingMeshResults(results, topoMap)

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
					Details: []rdma.PingMeshPairResult{
						{SrcDev: "ibp0", DstDev: "ibp0", Pass: false, Error: "timeout"},
					},
				},
				{
					Status: checks.StatusPass,
					Details: []rdma.PingMeshPairResult{
						{SrcDev: "ibp0", DstDev: "ibp0", Pass: true},
					},
				},
			},
		}
		report, failures := c.classifyPingMeshResults(results, topoMap)

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
					Details: []rdma.PingMeshPairResult{
						{SrcDev: "ibp0", DstDev: "ibp0", Pass: true},
					},
				},
			},
		}
		report, _ := c.classifyPingMeshResults(results, topoMap)

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
					Details: []rdma.PingMeshPairResult{
						{SrcDev: "ibp0", DstDev: "ibp0", Pass: true},
					},
				},
			},
		}
		report, _ := c.classifyPingMeshResults(results, singleTopoMap)

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
		got := pingMeshStatus(tt.passed, tt.total)
		if got != tt.want {
			t.Errorf("pingMeshStatus(%d, %d) = %q, want %q", tt.passed, tt.total, got, tt.want)
		}
	}
}

func TestBuildRailMap(t *testing.T) {
	topo := makeTopo("ibp0", "ibp1", "ibp2")
	m := buildRailMap(topo)
	if m["ibp0"] != 0 || m["ibp1"] != 1 || m["ibp2"] != 2 {
		t.Errorf("unexpected rail map: %v", m)
	}

	nilMap := buildRailMap(nil)
	if len(nilMap) != 0 {
		t.Errorf("buildRailMap(nil) should return empty map, got %v", nilMap)
	}
}

func makeNodeReportWithTopo(node string, topo *checks.NodeTopology) checks.NodeReport {
	return checks.NodeReport{
		Node: node,
		Results: []checks.Result{
			{
				Category: "networking_rdma",
				Name:     "gpu_nic_topology",
				Status:   checks.StatusPass,
				Message:  "2 GPU(s), 2 NIC(s), strategy=numa_affinity: GPU0↔mlx5_0, GPU1↔mlx5_1",
				Details:  topo,
			},
		},
	}
}

func TestApplyBandwidthPairing_Basic(t *testing.T) {
	topo := &checks.NodeTopology{
		GPUList: []checks.GPUInfo{
			{ID: 0, NUMA: 0, PCIAddr: "0001:00:00.0"},
			{ID: 1, NUMA: 0, PCIAddr: "0002:00:00.0"},
		},
		NICList: []checks.NICInfo{
			{Dev: "mlx5_0", NUMA: 0, PCIAddr: "0101:00:00.0"},
			{Dev: "mlx5_1", NUMA: 0, PCIAddr: "0102:00:00.0"},
		},
		Pairs: []checks.GPUNICPair{
			{GPU: checks.GPUInfo{ID: 0}, NIC: checks.NICInfo{Dev: "mlx5_0"}},
			{GPU: checks.GPUInfo{ID: 1}, NIC: checks.NICInfo{Dev: "mlx5_1"}},
		},
		PairingStrategy: checks.PairingNUMAAffinity,
		IsFlat:          true,
	}

	netReports := []checks.NodeReport{makeNodeReportWithTopo("node-1", topo)}
	bwResults := map[string]*rdma.LoopbackBWReport{
		"node-1": {
			Results: []rdma.LoopbackBWEntry{
				{GPUId: 0, NICDev: "mlx5_0", BWGbps: 427},
				{GPUId: 0, NICDev: "mlx5_1", BWGbps: 189},
				{GPUId: 1, NICDev: "mlx5_0", BWGbps: 189},
				{GPUId: 1, NICDev: "mlx5_1", BWGbps: 427},
			},
		},
	}

	c := &Controller{output: &bytes.Buffer{}}
	updated := c.applyBandwidthPairing(netReports, bwResults)

	updatedTopo := checks.ExtractTopology(updated[0])
	if updatedTopo == nil {
		t.Fatal("expected topology in updated report")
	}
	if updatedTopo.PairingStrategy != checks.PairingBandwidthProbe {
		t.Errorf("strategy = %q, want %q", updatedTopo.PairingStrategy, checks.PairingBandwidthProbe)
	}
	if len(updatedTopo.Pairs) != 2 {
		t.Fatalf("expected 2 pairs, got %d", len(updatedTopo.Pairs))
	}
	// Diagonal dominant: GPU0↔mlx5_0 (427), GPU1↔mlx5_1 (427)
	if updatedTopo.Pairs[0].GPU.ID != 0 || updatedTopo.Pairs[0].NIC.Dev != "mlx5_0" {
		t.Errorf("pair 0: expected GPU0↔mlx5_0, got GPU%d↔%s", updatedTopo.Pairs[0].GPU.ID, updatedTopo.Pairs[0].NIC.Dev)
	}
	if updatedTopo.Pairs[0].IntrahostBWGbps != 427 {
		t.Errorf("pair 0 BW = %f, want 427", updatedTopo.Pairs[0].IntrahostBWGbps)
	}
	if updatedTopo.Pairs[1].GPU.ID != 1 || updatedTopo.Pairs[1].NIC.Dev != "mlx5_1" {
		t.Errorf("pair 1: expected GPU1↔mlx5_1, got GPU%d↔%s", updatedTopo.Pairs[1].GPU.ID, updatedTopo.Pairs[1].NIC.Dev)
	}

	// Check that Result.Message was updated
	for _, res := range updated[0].Results {
		if res.Name == "gpu_nic_topology" {
			if !strings.Contains(res.Message, "intra-host_bandwidth") {
				t.Errorf("expected strategy in message, got: %s", res.Message)
			}
			break
		}
	}
}

func TestApplyBandwidthPairing_SkipsNonFlat(t *testing.T) {
	topo := &checks.NodeTopology{
		GPUList: []checks.GPUInfo{{ID: 0}},
		NICList: []checks.NICInfo{{Dev: "mlx5_0"}},
		Pairs:   []checks.GPUNICPair{{GPU: checks.GPUInfo{ID: 0}, NIC: checks.NICInfo{Dev: "mlx5_0"}}},
		PairingStrategy: checks.PairingPCIeDistance,
	}

	netReports := []checks.NodeReport{makeNodeReportWithTopo("node-1", topo)}
	bwResults := map[string]*rdma.LoopbackBWReport{
		"node-1": {
			Results: []rdma.LoopbackBWEntry{
				{GPUId: 0, NICDev: "mlx5_0", BWGbps: 400},
			},
		},
	}

	c := &Controller{output: &bytes.Buffer{}}
	updated := c.applyBandwidthPairing(netReports, bwResults)

	updatedTopo := checks.ExtractTopology(updated[0])
	if updatedTopo.PairingStrategy != checks.PairingPCIeDistance {
		t.Errorf("expected PCIe distance pairing to be preserved, got %q", updatedTopo.PairingStrategy)
	}
}

func TestApplyBandwidthPairing_EmptyBW(t *testing.T) {
	topo := &checks.NodeTopology{
		GPUList:         []checks.GPUInfo{{ID: 0}},
		NICList:         []checks.NICInfo{{Dev: "mlx5_0"}},
		Pairs:           []checks.GPUNICPair{{GPU: checks.GPUInfo{ID: 0}, NIC: checks.NICInfo{Dev: "mlx5_0"}}},
		PairingStrategy: checks.PairingNUMAAffinity,
		IsFlat:          true,
	}

	netReports := []checks.NodeReport{makeNodeReportWithTopo("node-1", topo)}
	bwResults := map[string]*rdma.LoopbackBWReport{
		"node-1": {Results: []rdma.LoopbackBWEntry{}},
	}

	c := &Controller{output: &bytes.Buffer{}}
	updated := c.applyBandwidthPairing(netReports, bwResults)

	// No BW entries => BandwidthOptimalPairing returns empty => keeps NUMA-affinity
	updatedTopo := checks.ExtractTopology(updated[0])
	if updatedTopo.PairingStrategy != checks.PairingNUMAAffinity {
		t.Errorf("expected NUMA-affinity fallback with empty BW data, got %q", updatedTopo.PairingStrategy)
	}
}
