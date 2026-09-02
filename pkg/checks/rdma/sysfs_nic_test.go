package rdma

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
)

func TestResolveLinkLayer(t *testing.T) {
	root := t.TempDir()
	efaDev := "rdmap181s0"
	pciDir := filepath.Join(root, efaDev, "device")
	if err := os.MkdirAll(pciDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pciDir, "vendor"), []byte("0x1d0f\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pciDir, "device"), []byte("0xefa1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldRoot := sysfsNICRoot
	sysfsNICRoot = root
	t.Cleanup(func() { sysfsNICRoot = oldRoot })

	tests := []struct {
		dev, raw string
		want     checks.LinkLayer
	}{
		{efaDev, "Unknown", checks.LinkLayerSRD},
		{"mlx5_0", "Unknown", checks.LinkLayerUnknown},
		{"mlx5_0", "InfiniBand", checks.LinkLayerInfiniBand},
		{"mlx5_0", "  Ethernet  ", checks.LinkLayerEthernet},
	}
	for _, tt := range tests {
		if got := resolveLinkLayer(tt.dev, tt.raw); got != tt.want {
			t.Errorf("resolveLinkLayer(%q, %q) = %q, want %q", tt.dev, tt.raw, got, tt.want)
		}
	}
}

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
