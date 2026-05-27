package checks

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestGPUNICPairMarshalJSON(t *testing.T) {
	pair := GPUNICPair{
		GPU:      GPUInfo{ID: 0, Name: "H100", NUMA: 0, PCIAddr: "0001:00:00.0", PCIePath: []string{"0001:00:00.0"}},
		NIC:      NICInfo{Dev: "mlx5_0", NUMA: 0, PCIAddr: "0101:00:00.0", LinkLayer: LinkLayerInfiniBand},
		PCIeHops: 3,
	}

	data, err := json.Marshal(pair)
	if err != nil {
		t.Fatalf("MarshalJSON failed: %v", err)
	}

	s := string(data)
	for _, want := range []string{`"gpu_id":0`, `"gpu_numa":0`, `"nic_dev":"mlx5_0"`, `"nic_numa":0`, `"pcie_hops":3`} {
		if !strings.Contains(s, want) {
			t.Errorf("expected %s in output, got: %s", want, s)
		}
	}
	for _, excluded := range []string{`"name"`, `"pci_addr"`, `"link_layer"`, `"pcie_path"`} {
		if strings.Contains(s, excluded) {
			t.Errorf("unexpected field %s in slim output: %s", excluded, s)
		}
	}
}

func TestGPUNICPairMarshalCrossNUMA(t *testing.T) {
	pair := GPUNICPair{
		GPU:      GPUInfo{ID: 4, NUMA: 1},
		NIC:      NICInfo{Dev: "mlx5_3", NUMA: 0},
		PCIeHops: 105,
	}

	data, err := json.Marshal(pair)
	if err != nil {
		t.Fatalf("MarshalJSON failed: %v", err)
	}

	s := string(data)
	if !strings.Contains(s, `"gpu_numa":1`) {
		t.Errorf("expected gpu_numa:1, got: %s", s)
	}
	if !strings.Contains(s, `"nic_numa":0`) {
		t.Errorf("expected nic_numa:0, got: %s", s)
	}
}

func TestGPUNICPairUnmarshalJSON(t *testing.T) {
	data := []byte(`{"gpu_id":2,"gpu_numa":1,"nic_dev":"mlx5_3","nic_numa":1,"pcie_hops":5}`)
	var pair GPUNICPair
	if err := json.Unmarshal(data, &pair); err != nil {
		t.Fatalf("UnmarshalJSON failed: %v", err)
	}
	if pair.GPU.ID != 2 {
		t.Errorf("expected GPU.ID=2, got %d", pair.GPU.ID)
	}
	if pair.GPU.NUMA != 1 {
		t.Errorf("expected GPU.NUMA=1, got %d", pair.GPU.NUMA)
	}
	if pair.NIC.Dev != "mlx5_3" {
		t.Errorf("expected NIC.Dev=mlx5_3, got %s", pair.NIC.Dev)
	}
	if pair.NIC.NUMA != 1 {
		t.Errorf("expected NIC.NUMA=1, got %d", pair.NIC.NUMA)
	}
	if pair.PCIeHops != 5 {
		t.Errorf("expected PCIeHops=5, got %d", pair.PCIeHops)
	}
}

func TestNodeTopologyJSONRoundTrip(t *testing.T) {
	topo := NodeTopology{
		GPUNICPCIeMapping: "0001:00:00.0=0101:00:00.0,0002:00:00.0=0102:00:00.0",
		GPUCount:          2,
		NICCount:          2,
		IsFlat:            true,
		PairingStrategy:   PairingNUMAAffinity,
		GPUList: []GPUInfo{
			{ID: 0, Name: "H100", NUMA: 0, PCIAddr: "0001:00:00.0"},
			{ID: 1, Name: "H100", NUMA: 1, PCIAddr: "0002:00:00.0"},
		},
		NICList: []NICInfo{
			{Dev: "mlx5_0", NUMA: 0, PCIAddr: "0101:00:00.0", LinkLayer: LinkLayerInfiniBand},
			{Dev: "mlx5_1", NUMA: 1, PCIAddr: "0102:00:00.0", LinkLayer: LinkLayerInfiniBand},
		},
		Pairs: []GPUNICPair{
			{GPU: GPUInfo{ID: 0, NUMA: 0}, NIC: NICInfo{Dev: "mlx5_0", NUMA: 0}, PCIeHops: 0},
			{GPU: GPUInfo{ID: 1, NUMA: 1}, NIC: NICInfo{Dev: "mlx5_1", NUMA: 1}, PCIeHops: 3},
		},
	}

	data, err := json.MarshalIndent(topo, "", "  ")
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	s := string(data)
	t.Logf("Marshaled JSON:\n%s", s)

	// gpu_nic_pcie_mapping appears before gpu_count
	mappingIdx := strings.Index(s, "gpu_nic_pcie_mapping")
	countIdx := strings.Index(s, "gpu_count")
	if mappingIdx < 0 || countIdx < 0 || mappingIdx > countIdx {
		t.Error("gpu_nic_pcie_mapping should appear before gpu_count in JSON output")
	}

	// Pairs are slim (no pci_addr after "pairs" key)
	pairsIdx := strings.Index(s, `"pairs"`)
	pciIdx := strings.LastIndex(s, `"pci_addr"`)
	if pciIdx > pairsIdx {
		t.Error("pairs should not contain pci_addr")
	}

	// Round-trip preserves all slim fields
	var topo2 NodeTopology
	if err := json.Unmarshal(data, &topo2); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if topo2.GPUNICPCIeMapping != topo.GPUNICPCIeMapping {
		t.Errorf("mapping mismatch: got %q", topo2.GPUNICPCIeMapping)
	}
	if len(topo2.Pairs) != 2 {
		t.Fatalf("expected 2 pairs, got %d", len(topo2.Pairs))
	}
	if topo2.Pairs[0].GPU.ID != 0 || topo2.Pairs[0].NIC.Dev != "mlx5_0" || topo2.Pairs[0].PCIeHops != 0 {
		t.Errorf("pair[0] mismatch: %+v", topo2.Pairs[0])
	}
	if topo2.Pairs[1].GPU.NUMA != 1 || topo2.Pairs[1].NIC.NUMA != 1 {
		t.Errorf("pair[1] NUMA mismatch: GPU.NUMA=%d, NIC.NUMA=%d", topo2.Pairs[1].GPU.NUMA, topo2.Pairs[1].NIC.NUMA)
	}
	if topo2.NICList[0].LinkLayer != LinkLayerInfiniBand {
		t.Errorf("NICList LinkLayer lost after round-trip: %q", topo2.NICList[0].LinkLayer)
	}
}

func TestNodeTopologyEmptyPairs(t *testing.T) {
	topo := NodeTopology{
		GPUCount: 2,
		NICCount: 0,
	}

	data, err := json.Marshal(topo)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	s := string(data)
	// gpu_nic_pcie_mapping should always be present (empty string, not omitted)
	if !strings.Contains(s, `"gpu_nic_pcie_mapping":""`) {
		t.Errorf("expected gpu_nic_pcie_mapping present even when empty, got: %s", s)
	}
}
