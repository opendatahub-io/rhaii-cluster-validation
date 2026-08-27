package rdma

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestListNICStatusFromSysfs_symlinkPort(t *testing.T) {
	root := t.TempDir()
	dev := "rdmap79s0"
	portsPath := filepath.Join(root, "sys", "class", "infiniband", dev, "ports", "1")
	if err := os.MkdirAll(portsPath, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, val string) {
		if err := os.WriteFile(filepath.Join(portsPath, name), []byte(val), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("state", "4: ACTIVE\n")
	write("phys_state", "5: LinkUp\n")
	write("rate", "100 Gb/sec (4X EDR)\n")
	write("link_layer", "Unknown\n")

	orig := sysfsNICRoot
	sysfsNICRoot = filepath.Join(root, "sys", "class", "infiniband")
	t.Cleanup(func() { sysfsNICRoot = orig })

	nics, err := listNICStatusFromSysfs(context.TODO(), []string{dev})
	if err != nil {
		t.Fatalf("listNICStatusFromSysfs() error = %v", err)
	}
	if len(nics) != 1 || nics[0].Name != dev || nics[0].State != "Active" {
		t.Fatalf("got %+v, want one active %s", nics, dev)
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
