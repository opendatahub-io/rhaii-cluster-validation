package rdma

import (
	"testing"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/config"
)

func makeTestGPU(id int, path ...string) checks.GPUInfo {
	return checks.GPUInfo{ID: id, PCIePath: path}
}

func makeTestNIC(dev string, path ...string) checks.NICInfo {
	return checks.NICInfo{Dev: dev, PCIePath: path}
}

func TestBuildPairs_multiNICPCIe(t *testing.T) {
	gpus := []checks.GPUInfo{
		makeTestGPU(0, "0000:37:00.0", "0000:38:00.0", "0000:3b:00.0"),
		makeTestGPU(1, "0000:37:00.0", "0000:38:00.0", "0000:4b:00.0"),
	}
	nics := []checks.NICInfo{
		makeTestNIC("rdmap79s0", "0000:37:00.0", "0000:38:00.0", "0000:3b:00.0", "0000:3c:00.0"),
		makeTestNIC("rdmap80s0", "0000:37:00.0", "0000:38:00.0", "0000:3b:00.0", "0000:3c:01.0"),
		makeTestNIC("rdmap96s0", "0000:37:00.0", "0000:38:00.0", "0000:4b:00.0", "0000:4c:00.0"),
		makeTestNIC("rdmap97s0", "0000:37:00.0", "0000:38:00.0", "0000:4b:00.0", "0000:4c:01.0"),
	}

	pairs, _, strategy := buildPairs(gpus, nics, config.RDMATypeSRD)
	if strategy != checks.PairingMultiNICPCIe {
		t.Fatalf("strategy = %q, want %q", strategy, checks.PairingMultiNICPCIe)
	}
	if len(pairs) != 4 {
		t.Fatalf("got %d pairs, want 4", len(pairs))
	}

	gpu0Count, gpu1Count := 0, 0
	for _, p := range pairs {
		switch p.GPU.ID {
		case 0:
			gpu0Count++
		case 1:
			gpu1Count++
		}
	}
	if gpu0Count != 2 || gpu1Count != 2 {
		t.Errorf("unexpected GPU distribution: gpu0=%d gpu1=%d", gpu0Count, gpu1Count)
	}

	for i := 1; i < len(pairs); i++ {
		prev, cur := pairs[i-1], pairs[i]
		if prev.GPU.ID > cur.GPU.ID || (prev.GPU.ID == cur.GPU.ID && prev.NIC.Dev > cur.NIC.Dev) {
			t.Errorf("pairs not sorted: %+v then %+v", prev, cur)
		}
	}
}

func TestBuildPairs_equalCountUsesPCIeDistance(t *testing.T) {
	gpus := []checks.GPUInfo{
		makeTestGPU(0, "0000:37:00.0", "0000:38:00.0", "0000:3b:00.0"),
		makeTestGPU(1, "0000:37:00.0", "0000:38:00.0", "0000:4b:00.0"),
	}
	nics := []checks.NICInfo{
		makeTestNIC("rdmap79s0", "0000:37:00.0", "0000:38:00.0", "0000:3c:00.0"),
		makeTestNIC("rdmap96s0", "0000:37:00.0", "0000:38:00.0", "0000:4c:00.0"),
	}

	_, _, strategy := buildPairs(gpus, nics, config.RDMATypeSRD)
	if strategy != checks.PairingPCIeDistance {
		t.Errorf("strategy = %q, want %q for 1:1 GPU:NIC", strategy, checks.PairingPCIeDistance)
	}
}
