package rdma

import "testing"

func TestIsEFAPCI(t *testing.T) {
	tests := []struct {
		vendor, device string
		want           bool
	}{
		{"0x1d0f", "0xefa1", true},
		{"0x1d0f", "0xefa0", true},
		{"0x1d0f", "0xefa4", true},
		{"0x15b3", "0xefa1", false},
		{"0x1d0f", "0x1017", false},
	}
	for _, tt := range tests {
		if got := isEFAPCI(tt.vendor, tt.device); got != tt.want {
			t.Errorf("isEFAPCI(%q, %q) = %v, want %v", tt.vendor, tt.device, got, tt.want)
		}
	}
}
