package rdma

import (
	"bytes"
	"strings"
	"testing"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
)

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

func TestBuildTopologyMap(t *testing.T) {
	topo := &checks.NodeTopology{
		GPUList: []checks.GPUInfo{{ID: 0}},
		NICList: []checks.NICInfo{{Dev: "mlx5_0"}},
	}
	reports := []checks.NodeReport{
		makeNodeReportWithTopo("node-1", topo),
		{Node: "node-2", Results: []checks.Result{{Category: "gpu_hardware", Name: "gpu_driver_version", Status: checks.StatusPass}}},
	}

	m := BuildTopologyMap(reports)

	if len(m) != 1 {
		t.Fatalf("expected 1 entry (node-2 has no topology), got %d", len(m))
	}
	if _, ok := m["node-1"]; !ok {
		t.Error("expected topology for node-1")
	}
	if _, ok := m["node-2"]; ok {
		t.Error("did not expect topology for node-2 (no gpu_nic_topology result)")
	}
}

func TestTopologyCoversAllNodes(t *testing.T) {
	topo := &checks.NodeTopology{GPUList: []checks.GPUInfo{{ID: 0}}}
	reports := []checks.NodeReport{
		makeNodeReportWithTopo("node-1", topo),
		makeNodeReportWithTopo("node-2", topo),
	}

	if !TopologyCoversAllNodes(reports, []string{"node-1", "node-2"}) {
		t.Error("expected coverage to be complete")
	}
	if TopologyCoversAllNodes(reports, []string{"node-1", "node-2", "node-3"}) {
		t.Error("expected coverage to be incomplete (node-3 missing)")
	}
	if !TopologyCoversAllNodes(reports, nil) {
		t.Error("expected empty gpuNodes to trivially be covered")
	}
}

func TestNeedsBandwidthProbe(t *testing.T) {
	numaTopo := &checks.NodeTopology{PairingStrategy: checks.PairingNUMAAffinity}
	pcieTopo := &checks.NodeTopology{PairingStrategy: checks.PairingPCIeDistance}

	if NeedsBandwidthProbe(nil) {
		t.Error("nil reports should not need a BW probe")
	}
	if NeedsBandwidthProbe([]checks.NodeReport{makeNodeReportWithTopo("node-1", pcieTopo)}) {
		t.Error("PCIe-distance pairing should not need a BW probe")
	}
	if !NeedsBandwidthProbe([]checks.NodeReport{makeNodeReportWithTopo("node-1", numaTopo)}) {
		t.Error("NUMA-affinity pairing should need a BW probe")
	}
	// Mixed: one node NUMA, one PCIe — still needs a probe.
	mixed := []checks.NodeReport{
		makeNodeReportWithTopo("node-1", pcieTopo),
		makeNodeReportWithTopo("node-2", numaTopo),
	}
	if !NeedsBandwidthProbe(mixed) {
		t.Error("expected BW probe needed when any node uses NUMA-affinity pairing")
	}
}

func TestWarnUnpairedFlatNodes(t *testing.T) {
	flatUnpaired := &checks.NodeTopology{IsFlat: true, PairingStrategy: checks.PairingNUMAAffinity}
	flatPaired := &checks.NodeTopology{IsFlat: true, PairingStrategy: checks.PairingBandwidthProbe}
	nonFlat := &checks.NodeTopology{IsFlat: false, PairingStrategy: checks.PairingNUMAAffinity}

	reports := []checks.NodeReport{
		makeNodeReportWithTopo("flat-unpaired", flatUnpaired),
		makeNodeReportWithTopo("flat-paired", flatPaired),
		makeNodeReportWithTopo("non-flat", nonFlat),
	}

	updated := WarnUnpairedFlatNodes(reports)

	statusFor := func(node string) (checks.Status, string) {
		for _, r := range updated {
			if r.Node != node {
				continue
			}
			for _, res := range r.Results {
				if res.Name == "gpu_nic_topology" {
					return res.Status, res.Message
				}
			}
		}
		t.Fatalf("no gpu_nic_topology result found for %s", node)
		return "", ""
	}

	if status, msg := statusFor("flat-unpaired"); status != checks.StatusWarn {
		t.Errorf("flat-unpaired: status = %q, want WARN (message: %s)", status, msg)
	} else if !strings.Contains(msg, "BW probe unavailable") {
		t.Errorf("flat-unpaired: expected fallback note in message, got: %s", msg)
	}
	if status, _ := statusFor("flat-paired"); status != checks.StatusPass {
		t.Errorf("flat-paired: status = %q, want PASS (already bandwidth-paired)", status)
	}
	if status, _ := statusFor("non-flat"); status != checks.StatusPass {
		t.Errorf("non-flat: status = %q, want PASS (not a flat topology)", status)
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
	bwResults := map[string]*LoopbackBWReport{
		"node-1": {
			Results: []LoopbackBWEntry{
				{GPUId: 0, NICDev: "mlx5_0", BWGbps: 427},
				{GPUId: 0, NICDev: "mlx5_1", BWGbps: 189},
				{GPUId: 1, NICDev: "mlx5_0", BWGbps: 189},
				{GPUId: 1, NICDev: "mlx5_1", BWGbps: 427},
			},
		},
	}

	updated := ApplyBandwidthPairing(netReports, bwResults, &bytes.Buffer{})

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
		GPUList:         []checks.GPUInfo{{ID: 0}},
		NICList:         []checks.NICInfo{{Dev: "mlx5_0"}},
		Pairs:           []checks.GPUNICPair{{GPU: checks.GPUInfo{ID: 0}, NIC: checks.NICInfo{Dev: "mlx5_0"}}},
		PairingStrategy: checks.PairingPCIeDistance,
	}

	netReports := []checks.NodeReport{makeNodeReportWithTopo("node-1", topo)}
	bwResults := map[string]*LoopbackBWReport{
		"node-1": {
			Results: []LoopbackBWEntry{
				{GPUId: 0, NICDev: "mlx5_0", BWGbps: 400},
			},
		},
	}

	updated := ApplyBandwidthPairing(netReports, bwResults, &bytes.Buffer{})

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
	bwResults := map[string]*LoopbackBWReport{
		"node-1": {Results: []LoopbackBWEntry{}},
	}

	updated := ApplyBandwidthPairing(netReports, bwResults, &bytes.Buffer{})

	// No BW entries => BandwidthOptimalPairing returns empty => keeps NUMA-affinity
	updatedTopo := checks.ExtractTopology(updated[0])
	if updatedTopo.PairingStrategy != checks.PairingNUMAAffinity {
		t.Errorf("expected NUMA-affinity fallback with empty BW data, got %q", updatedTopo.PairingStrategy)
	}
}
