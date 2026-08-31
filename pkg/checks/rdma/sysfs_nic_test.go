package rdma

import (
	"testing"
)

func TestParseSysfsState(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"4: ACTIVE", "Active"},
		{"1: DOWN", "Down"},
		{"", "Unknown"},
	}
	for _, tt := range tests {
		if got := parseSysfsState(tt.in); got != tt.want {
			t.Errorf("parseSysfsState(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestIsPortActive(t *testing.T) {
	if !isPortActive("4: ACTIVE", "5: LinkUp") {
		t.Error("expected active port")
	}
	if isPortActive("4: ACTIVE", "3: Disabled") {
		t.Error("expected inactive when phys state not LinkUp")
	}
	if isPortActive("1: DOWN", "5: LinkUp") {
		t.Error("expected inactive when state not ACTIVE")
	}
}

func TestParseSysfsRate(t *testing.T) {
	if got := parseSysfsRate("100 Gb/sec (4X EDR)"); got != "100" {
		t.Errorf("parseSysfsRate = %q, want 100", got)
	}
}

func TestPortStatusName(t *testing.T) {
	if got := portStatusName("mlx5_0", "1"); got != "mlx5_0" {
		t.Errorf("single port name = %q, want mlx5_0", got)
	}
	if got := portStatusName("mlx5_0", "2"); got != "mlx5_0/port2" {
		t.Errorf("multi port name = %q, want mlx5_0/port2", got)
	}
}
