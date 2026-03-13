package config

import (
	"testing"
)

func TestGetConfig_AllPlatforms(t *testing.T) {
	platforms := []Platform{PlatformAKS, PlatformEKS, PlatformCoreWeave, PlatformOCP}

	for _, p := range platforms {
		t.Run(string(p), func(t *testing.T) {
			cfg, err := GetConfig(p)
			if err != nil {
				t.Fatalf("GetConfig(%s) returned error: %v", p, err)
			}

			if cfg.Platform != p {
				t.Errorf("expected platform %s, got %s", p, cfg.Platform)
			}

			// GPU config must have supported types
			if len(cfg.GPU.SupportedTypes) == 0 {
				t.Error("GPU.SupportedTypes must not be empty")
			}

			// Must have a device plugin
			if cfg.GPU.DevicePlugin == "" {
				t.Error("GPU.DevicePlugin must not be empty")
			}

			// Must have nvidia paths
			if cfg.GPU.NvidiaSmiPath == "" {
				t.Error("GPU.NvidiaSmiPath must not be empty")
			}

			// Must have RDMA config
			if cfg.RDMA.NICPrefix == "" {
				t.Error("RDMA.NICPrefix must not be empty")
			}

			// Thresholds must be positive
			if cfg.Thresholds.TCPBandwidth.Pass <= 0 {
				t.Error("TCPBandwidth.Pass must be positive")
			}
			if cfg.Thresholds.RDMABandwidthPD.Pass <= 0 {
				t.Error("RDMABandwidthPD.Pass must be positive")
			}
			if cfg.Thresholds.RDMABandwidthWEP.Pass <= 0 {
				t.Error("RDMABandwidthWEP.Pass must be positive")
			}

			// Pass > Warn > Fail for bandwidth
			if cfg.Thresholds.TCPBandwidth.Pass <= cfg.Thresholds.TCPBandwidth.Warn {
				t.Error("TCPBandwidth: Pass must be > Warn")
			}
			if cfg.Thresholds.TCPBandwidth.Warn <= cfg.Thresholds.TCPBandwidth.Fail {
				t.Error("TCPBandwidth: Warn must be > Fail")
			}
		})
	}
}

func TestGetConfig_UnknownPlatform(t *testing.T) {
	cfg, err := GetConfig(PlatformUnknown)
	if err != nil {
		t.Fatalf("GetConfig(Unknown) returned error: %v", err)
	}

	// Should fall back to AKS defaults
	if len(cfg.GPU.SupportedTypes) == 0 {
		t.Error("Unknown platform should fall back to AKS defaults")
	}
}

func TestGetConfig_EKS_UsesEFA(t *testing.T) {
	cfg, err := GetConfig(PlatformEKS)
	if err != nil {
		t.Fatalf("GetConfig(EKS) returned error: %v", err)
	}

	if cfg.RDMA.DevicePlugin != "efa-device-plugin" {
		t.Errorf("EKS should use efa-device-plugin, got %s", cfg.RDMA.DevicePlugin)
	}
	if cfg.RDMA.NICPrefix != "efa_" {
		t.Errorf("EKS should use efa_ NIC prefix, got %s", cfg.RDMA.NICPrefix)
	}
}

func TestGetConfig_AKS_HasUniqueTaint(t *testing.T) {
	cfg, err := GetConfig(PlatformAKS)
	if err != nil {
		t.Fatalf("GetConfig(AKS) returned error: %v", err)
	}

	if cfg.GPU.TaintKey != "sku" {
		t.Errorf("AKS should use taint key 'sku', got %s", cfg.GPU.TaintKey)
	}
	if cfg.GPU.TaintValue != "gpu" {
		t.Errorf("AKS should use taint value 'gpu', got %s", cfg.GPU.TaintValue)
	}
}
