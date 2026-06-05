package rdma

import (
	"strconv"
	"strings"
	"testing"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
)

func TestBuildLoopbackScript(t *testing.T) {
	script, err := BuildLoopbackScript([]int{0, 1}, []string{"mlx5_0", "mlx5_1"}, 5000, 1048576, 30, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(script, "GPU_IDS='0,1'") {
		t.Error("expected GPU_IDS='0,1' in script")
	}
	if !strings.Contains(script, "NIC_DEVS='mlx5_0,mlx5_1'") {
		t.Error("expected NIC_DEVS in script")
	}
	if !strings.Contains(script, "iters=5000") {
		t.Error("expected iters=5000")
	}
	if !strings.Contains(script, "msg_size=1048576") {
		t.Error("expected msg_size=1048576")
	}
	if !strings.Contains(script, "per_test_timeout=30") {
		t.Error("expected per_test_timeout=30")
	}
	if !strings.Contains(script, "ib_write_bw") {
		t.Error("expected ib_write_bw command in script")
	}
	if !strings.Contains(script, "timeout") {
		t.Error("expected timeout wrapper in script")
	}
	if !strings.Contains(script, "--use_cuda") {
		t.Error("expected --use_cuda in script")
	}
	if !strings.Contains(script, "--out_json") {
		t.Error("expected --out_json in script")
	}
	if !strings.Contains(script, "--out_json_file") {
		t.Error("expected --out_json_file in script")
	}
	if !strings.Contains(script, "num_qps=4") {
		t.Error("expected num_qps=4 (needed to saturate 400G NICs)")
	}
	if !strings.Contains(script, "-q \"$num_qps\"") {
		t.Error("expected -q flag using $num_qps variable")
	}
}

func TestBuildLoopbackScriptDefaults(t *testing.T) {
	script, err := BuildLoopbackScript([]int{0}, []string{"mlx5_0"}, 0, 0, 0, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(script, "iters=5000") {
		t.Error("expected default iters=5000")
	}
	if !strings.Contains(script, "msg_size=1048576") {
		t.Error("expected default msg_size")
	}
	if !strings.Contains(script, "per_test_timeout=30") {
		t.Error("expected default per_test_timeout")
	}
	if !strings.Contains(script, "num_qps=4") {
		t.Error("expected default num_qps=4")
	}
}

func TestBuildLoopbackScriptCustomQPs(t *testing.T) {
	script, err := BuildLoopbackScript([]int{0}, []string{"mlx5_0"}, 0, 0, 0, 8)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(script, "num_qps=8") {
		t.Error("expected num_qps=8 for custom QPs override")
	}
}

func TestBuildLoopbackScriptInvalidNICName(t *testing.T) {
	_, err := BuildLoopbackScript([]int{0}, []string{"-flag"}, 0, 0, 0, 0)
	if err == nil {
		t.Error("expected error for invalid NIC name '-flag'")
	}

	_, err = BuildLoopbackScript([]int{0}, []string{"mlx5_0; rm -rf /"}, 0, 0, 0, 0)
	if err == nil {
		t.Error("expected error for NIC name with shell metacharacters")
	}

	_, err = BuildLoopbackScript([]int{0}, []string{"mlx5_0"}, 0, 0, 0, 0)
	if err != nil {
		t.Errorf("unexpected error for valid NIC name: %v", err)
	}
}

func TestBuildLoopbackScriptEmptyInputs(t *testing.T) {
	_, err := BuildLoopbackScript([]int{}, []string{"mlx5_0"}, 0, 0, 0, 0)
	if err == nil {
		t.Error("expected error for empty GPU IDs")
	}

	_, err = BuildLoopbackScript([]int{0}, []string{}, 0, 0, 0, 0)
	if err == nil {
		t.Error("expected error for empty NIC devices")
	}

	_, err = BuildLoopbackScript(nil, nil, 0, 0, 0, 0)
	if err == nil {
		t.Error("expected error for nil inputs")
	}
}

func TestParseLoopbackBWOutput(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    int
		wantErr bool
	}{
		{
			name: "valid output",
			input: `some stderr junk
{"results":[{"gpu_id":0,"nic_dev":"mlx5_0","bw_gbps":427.09},{"gpu_id":0,"nic_dev":"mlx5_1","bw_gbps":189.62}]}`,
			want: 2,
		},
		{
			name: "with error entries",
			input: `{"results":[{"gpu_id":0,"nic_dev":"mlx5_0","bw_gbps":427.09},{"gpu_id":1,"nic_dev":"mlx5_0","bw_gbps":0,"error":"timeout after 30s"}]}`,
			want: 2,
		},
		{
			name: "ib_write_bw output mixed in",
			input: `Perftest v4.5
 #bytes     #iterations    BW peak[MiB/sec]    BW average[MiB/sec]   MsgRate[Mpps]
 1048576    5000             15128.13            15121.79              0.015122
---------------------------------------------------------------------------------------
deallocating GPU buffer 00007fdfc7e00000
destroying current CUDA Ctx
{"results":[{"gpu_id":0,"nic_dev":"mlx5_0","bw_gbps":427.09}]}`,
			want: 1,
		},
		{
			name:    "no JSON",
			input:   "just some logs with no json at all",
			wantErr: true,
		},
		{
			name:    "empty results",
			input:   `{"results":[]}`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entries, err := ParseLoopbackBWOutput(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(entries) != tt.want {
				t.Errorf("got %d entries, want %d", len(entries), tt.want)
			}
		})
	}
}

func TestParseLoopbackBWOutputFields(t *testing.T) {
	input := `{"results":[{"gpu_id":3,"nic_dev":"mlx5_2","bw_gbps":427.09},{"gpu_id":4,"nic_dev":"mlx5_5","bw_gbps":0,"error":"exit code 1"}]}`

	entries, err := ParseLoopbackBWOutput(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if entries[0].GPUId != 3 || entries[0].NICDev != "mlx5_2" || entries[0].BWGbps != 427.09 {
		t.Errorf("entry 0 mismatch: %+v", entries[0])
	}
	if entries[1].GPUId != 4 || entries[1].NICDev != "mlx5_5" || entries[1].BWGbps != 0 || entries[1].Error != "exit code 1" {
		t.Errorf("entry 1 mismatch: %+v", entries[1])
	}
}

func makeGPUs(ids ...int) []checks.GPUInfo {
	gpus := make([]checks.GPUInfo, len(ids))
	for i, id := range ids {
		gpus[i] = checks.GPUInfo{
			ID:      id,
			NUMA:    0,
			PCIAddr: "0000:00:00.0",
		}
	}
	return gpus
}

func makeNICs(devs ...string) []checks.NICInfo {
	nics := make([]checks.NICInfo, len(devs))
	for i, d := range devs {
		nics[i] = checks.NICInfo{
			Dev:     d,
			NUMA:    0,
			PCIAddr: "0000:00:00.0",
		}
	}
	return nics
}

func TestBandwidthOptimalPairing_BasicDiagonal(t *testing.T) {
	// Simulates the real-world 8x8 matrix where GPU_i pairs best with NIC_i
	entries := []LoopbackBWEntry{
		{GPUId: 0, NICDev: "mlx5_0", BWGbps: 427},
		{GPUId: 0, NICDev: "mlx5_1", BWGbps: 189},
		{GPUId: 1, NICDev: "mlx5_0", BWGbps: 189},
		{GPUId: 1, NICDev: "mlx5_1", BWGbps: 427},
	}
	gpus := makeGPUs(0, 1)
	nics := makeNICs("mlx5_0", "mlx5_1")

	pairs := BandwidthOptimalPairing(entries, gpus, nics)
	if len(pairs) != 2 {
		t.Fatalf("expected 2 pairs, got %d", len(pairs))
	}

	// Sorted by GPU ID
	if pairs[0].GPU.ID != 0 || pairs[0].NIC.Dev != "mlx5_0" {
		t.Errorf("pair 0: expected GPU0↔mlx5_0, got GPU%d↔%s", pairs[0].GPU.ID, pairs[0].NIC.Dev)
	}
	if pairs[1].GPU.ID != 1 || pairs[1].NIC.Dev != "mlx5_1" {
		t.Errorf("pair 1: expected GPU1↔mlx5_1, got GPU%d↔%s", pairs[1].GPU.ID, pairs[1].NIC.Dev)
	}
}

func TestBandwidthOptimalPairing_FailedTests(t *testing.T) {
	// mlx5_0 is broken for GPU0 (timeout), so GPU0 should pair with mlx5_1
	entries := []LoopbackBWEntry{
		{GPUId: 0, NICDev: "mlx5_0", BWGbps: 0, Error: "timeout after 30s"},
		{GPUId: 0, NICDev: "mlx5_1", BWGbps: 189},
		{GPUId: 1, NICDev: "mlx5_0", BWGbps: 427},
		{GPUId: 1, NICDev: "mlx5_1", BWGbps: 189},
	}
	gpus := makeGPUs(0, 1)
	nics := makeNICs("mlx5_0", "mlx5_1")

	pairs := BandwidthOptimalPairing(entries, gpus, nics)
	if len(pairs) != 2 {
		t.Fatalf("expected 2 pairs, got %d", len(pairs))
	}

	// GPU1 gets mlx5_0 (427 Gbps), GPU0 gets mlx5_1 (189 Gbps)
	if pairs[0].GPU.ID != 0 || pairs[0].NIC.Dev != "mlx5_1" {
		t.Errorf("pair 0: expected GPU0↔mlx5_1, got GPU%d↔%s", pairs[0].GPU.ID, pairs[0].NIC.Dev)
	}
	if pairs[1].GPU.ID != 1 || pairs[1].NIC.Dev != "mlx5_0" {
		t.Errorf("pair 1: expected GPU1↔mlx5_0, got GPU%d↔%s", pairs[1].GPU.ID, pairs[1].NIC.Dev)
	}
}

func TestBandwidthOptimalPairing_MoreGPUsThanNICs(t *testing.T) {
	entries := []LoopbackBWEntry{
		{GPUId: 0, NICDev: "mlx5_0", BWGbps: 427},
		{GPUId: 1, NICDev: "mlx5_0", BWGbps: 189},
		{GPUId: 2, NICDev: "mlx5_0", BWGbps: 189},
	}
	gpus := makeGPUs(0, 1, 2)
	nics := makeNICs("mlx5_0")

	pairs := BandwidthOptimalPairing(entries, gpus, nics)
	if len(pairs) != 3 {
		t.Fatalf("expected 3 pairs (1 primary + 2 overflow), got %d", len(pairs))
	}

	// All GPUs should be paired with mlx5_0 (the only NIC)
	for _, p := range pairs {
		if p.NIC.Dev != "mlx5_0" {
			t.Errorf("GPU%d should be paired with mlx5_0, got %s", p.GPU.ID, p.NIC.Dev)
		}
	}
}

func TestBandwidthOptimalPairing_Phase2PicksHighestBW(t *testing.T) {
	// 3 GPUs, 2 NICs. Phase 1 assigns GPU0↔mlx5_0 (427), GPU1↔mlx5_1 (300).
	// Phase 2 should assign GPU2 to mlx5_0 (200 > 150 for mlx5_1).
	entries := []LoopbackBWEntry{
		{GPUId: 0, NICDev: "mlx5_0", BWGbps: 427},
		{GPUId: 0, NICDev: "mlx5_1", BWGbps: 100},
		{GPUId: 1, NICDev: "mlx5_0", BWGbps: 189},
		{GPUId: 1, NICDev: "mlx5_1", BWGbps: 300},
		{GPUId: 2, NICDev: "mlx5_0", BWGbps: 200},
		{GPUId: 2, NICDev: "mlx5_1", BWGbps: 150},
	}
	gpus := makeGPUs(0, 1, 2)
	nics := makeNICs("mlx5_0", "mlx5_1")

	pairs := BandwidthOptimalPairing(entries, gpus, nics)
	if len(pairs) != 3 {
		t.Fatalf("expected 3 pairs, got %d", len(pairs))
	}

	// GPU2 (leftover) should pick mlx5_0 (200 Gbps > 150 for mlx5_1)
	if pairs[2].GPU.ID != 2 || pairs[2].NIC.Dev != "mlx5_0" {
		t.Errorf("GPU2: expected mlx5_0 (highest BW), got %s", pairs[2].NIC.Dev)
	}

	// Phase 1 pairs should have bandwidth set from the matched entry
	if pairs[0].IntrahostBWGbps != 427 {
		t.Errorf("GPU0 BW = %f, want 427 (Phase 1)", pairs[0].IntrahostBWGbps)
	}
	if pairs[1].IntrahostBWGbps != 300 {
		t.Errorf("GPU1 BW = %f, want 300 (Phase 1)", pairs[1].IntrahostBWGbps)
	}
	// Phase 2 pair should have bandwidth set from GPU2↔mlx5_0 entry
	if pairs[2].IntrahostBWGbps != 200 {
		t.Errorf("GPU2 BW = %f, want 200 (Phase 2)", pairs[2].IntrahostBWGbps)
	}
}

func TestBandwidthOptimalPairing_Empty(t *testing.T) {
	pairs := BandwidthOptimalPairing(nil, nil, nil)
	if pairs != nil {
		t.Errorf("expected nil pairs for empty input, got %d", len(pairs))
	}

	pairs = BandwidthOptimalPairing([]LoopbackBWEntry{}, makeGPUs(0), makeNICs("mlx5_0"))
	if len(pairs) != 0 {
		t.Errorf("expected 0 pairs when no BW entries, got %d", len(pairs))
	}
}

func TestBandwidthOptimalPairing_8x8Matrix(t *testing.T) {
	// Real-world data: diagonal has ~427 Gbps, off-diagonal ~189 Gbps
	var entries []LoopbackBWEntry
	for gpu := 0; gpu < 8; gpu++ {
		for nic := 0; nic < 8; nic++ {
			dev := "mlx5_" + strconv.Itoa(nic)
			bw := 189.0
			if gpu == nic {
				bw = 427.0
			}
			entries = append(entries, LoopbackBWEntry{GPUId: gpu, NICDev: dev, BWGbps: bw})
		}
	}

	gpus := makeGPUs(0, 1, 2, 3, 4, 5, 6, 7)
	devs := make([]string, 8)
	for i := 0; i < 8; i++ {
		devs[i] = "mlx5_" + strconv.Itoa(i)
	}
	nics := makeNICs(devs...)

	pairs := BandwidthOptimalPairing(entries, gpus, nics)
	if len(pairs) != 8 {
		t.Fatalf("expected 8 pairs, got %d", len(pairs))
	}

	// Each GPU_i should be paired with mlx5_i (the diagonal is optimal)
	for _, p := range pairs {
		expectedNIC := "mlx5_" + strconv.Itoa(p.GPU.ID)
		if p.NIC.Dev != expectedNIC {
			t.Errorf("GPU%d: expected %s, got %s", p.GPU.ID, expectedNIC, p.NIC.Dev)
		}
	}
}

func TestBuildGPUNICPCIeMapping(t *testing.T) {
	pairs := []checks.GPUNICPair{
		{GPU: checks.GPUInfo{ID: 0, PCIAddr: "0001:00:00.0"}, NIC: checks.NICInfo{Dev: "mlx5_0", PCIAddr: "0101:00:00.0"}},
		{GPU: checks.GPUInfo{ID: 1, PCIAddr: "0002:00:00.0"}, NIC: checks.NICInfo{Dev: "mlx5_1", PCIAddr: "0102:00:00.0"}},
	}
	got := BuildGPUNICPCIeMapping(pairs)
	want := "0001:00:00.0=0101:00:00.0,0002:00:00.0=0102:00:00.0"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildGPUNICPCIeMappingEmpty(t *testing.T) {
	got := BuildGPUNICPCIeMapping(nil)
	if got != "" {
		t.Errorf("expected empty string for nil pairs, got %q", got)
	}
}
