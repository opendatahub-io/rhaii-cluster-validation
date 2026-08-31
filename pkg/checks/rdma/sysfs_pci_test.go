package rdma

import "testing"

func TestNormalizePCIID(t *testing.T) {
	if got := normalizePCIID("1d0f\n"); got != "0x1d0f" {
		t.Errorf("normalizePCIID = %q, want 0x1d0f", got)
	}
}
