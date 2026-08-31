package rdma

import (
	"testing"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
)

func TestFormatPairsCompact(t *testing.T) {
	pairs := []checks.GPUNICPair{
		{GPU: checks.GPUInfo{ID: 0}, NIC: checks.NICInfo{Dev: "rdmap79s0"}},
		{GPU: checks.GPUInfo{ID: 0}, NIC: checks.NICInfo{Dev: "rdmap80s0"}},
		{GPU: checks.GPUInfo{ID: 0}, NIC: checks.NICInfo{Dev: "rdmap81s0"}},
		{GPU: checks.GPUInfo{ID: 0}, NIC: checks.NICInfo{Dev: "rdmap82s0"}},
	}
	got := FormatPairsCompact(pairs, checks.PairingMultiNICPCIe)
	want := "GPU0↔rdmap79s0-82s0 (×4)"
	if got != want {
		t.Errorf("FormatPairsCompact = %q, want %q", got, want)
	}
}

func TestFormatPairsCompact_singlePair(t *testing.T) {
	pairs := []checks.GPUNICPair{
		{GPU: checks.GPUInfo{ID: 0}, NIC: checks.NICInfo{Dev: "mlx5_0"}},
	}
	got := FormatPairsCompact(pairs, checks.PairingPCIeDistance)
	want := "GPU0↔mlx5_0"
	if got != want {
		t.Errorf("FormatPairsCompact = %q, want %q", got, want)
	}
}
