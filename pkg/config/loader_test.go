package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_DefaultsOnly(t *testing.T) {
	cfg, err := Load(PlatformAKS, "")
	if err != nil {
		t.Fatalf("Load(AKS, '') returned error: %v", err)
	}

	if cfg.Platform != PlatformAKS {
		t.Errorf("expected AKS, got %s", cfg.Platform)
	}
	if cfg.GPU.TaintKey != "sku" {
		t.Errorf("expected taint key 'sku', got %s", cfg.GPU.TaintKey)
	}
}

func TestLoad_WithOverrideFile(t *testing.T) {
	// Create temp override file
	dir := t.TempDir()
	overrideFile := filepath.Join(dir, "override.yaml")

	content := `
gpu:
  min_driver_version: "550.0"
  supported_types: ["H200", "B200"]
thresholds:
  tcp_bandwidth_gbps:
    pass: 50
`
	if err := os.WriteFile(overrideFile, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(PlatformAKS, overrideFile)
	if err != nil {
		t.Fatalf("Load with override returned error: %v", err)
	}

	// Overridden fields
	if cfg.GPU.MinDriverVersion != "550.0" {
		t.Errorf("expected min driver 550.0, got %s", cfg.GPU.MinDriverVersion)
	}
	if len(cfg.GPU.SupportedTypes) != 2 || cfg.GPU.SupportedTypes[0] != "H200" {
		t.Errorf("expected [H200, B200], got %v", cfg.GPU.SupportedTypes)
	}
	if cfg.Thresholds.TCPBandwidth.Pass != 50 {
		t.Errorf("expected TCP pass 50, got %f", cfg.Thresholds.TCPBandwidth.Pass)
	}

	// Non-overridden fields should keep defaults
	if cfg.GPU.TaintKey != "sku" {
		t.Errorf("taint key should remain 'sku', got %s", cfg.GPU.TaintKey)
	}
	if cfg.RDMA.NICPrefix != "mlx5_" {
		t.Errorf("NIC prefix should remain 'mlx5_', got %s", cfg.RDMA.NICPrefix)
	}
	if cfg.Thresholds.RDMABandwidthPD.Pass != 180 {
		t.Errorf("RDMA PD pass should remain 180, got %f", cfg.Thresholds.RDMABandwidthPD.Pass)
	}
}

func TestLoad_InvalidFile(t *testing.T) {
	_, err := Load(PlatformAKS, "/nonexistent/path.yaml")
	if err == nil {
		t.Error("expected error for nonexistent file")
	}
}

func TestLoad_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	badFile := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(badFile, []byte("{{invalid yaml"), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := Load(PlatformAKS, badFile)
	if err == nil {
		t.Error("expected error for invalid YAML")
	}
}
