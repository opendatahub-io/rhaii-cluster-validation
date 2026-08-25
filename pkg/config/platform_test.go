package config

import (
	"testing"

	"gopkg.in/yaml.v3"
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

			// Must have min driver version
			if cfg.GPU.MinDriverVersion == "" {
				t.Error("GPU.MinDriverVersion must not be empty")
			}

			// Agent must have resource requests
			if len(cfg.Agent.Requests) == 0 {
				t.Error("Agent.Requests must not be empty")
			}
			if cfg.Agent.Requests["cpu"] == "" {
				t.Error("Agent.Requests must have cpu")
			}
			if cfg.Agent.Requests["memory"] == "" {
				t.Error("Agent.Requests must have memory")
			}

			// Jobs must have resource requests
			if len(cfg.Jobs.Requests) == 0 {
				t.Error("Jobs.Requests must not be empty")
			}
			if cfg.Jobs.Requests["cpu"] == "" {
				t.Error("Jobs.Requests must have cpu")
			}
			if cfg.Jobs.Requests["memory"] == "" {
				t.Error("Jobs.Requests must have memory")
			}

			// Thresholds must be positive
			if cfg.Thresholds.TCPBandwidth.Pass <= 0 {
				t.Error("TCPBandwidth.Pass must be positive")
			}
			if cfg.Thresholds.TCPBandwidth.Warn <= 0 {
				t.Error("TCPBandwidth.Warn must be positive")
			}
			if cfg.Thresholds.RDMABandwidthPD.Pass <= 0 {
				t.Error("RDMABandwidthPD.Pass must be positive")
			}
			if cfg.Thresholds.RDMABandwidthPD.Warn <= 0 {
				t.Error("RDMABandwidthPD.Warn must be positive")
			}

			// Pass > Warn for bandwidth (higher is better)
			if cfg.Thresholds.TCPBandwidth.Pass <= cfg.Thresholds.TCPBandwidth.Warn {
				t.Error("TCPBandwidth: Pass must be > Warn")
			}
			if cfg.Thresholds.RDMABandwidthPD.Pass <= cfg.Thresholds.RDMABandwidthPD.Warn {
				t.Error("RDMABandwidthPD: Pass must be > Warn")
			}

			// Pass < Warn for latency (lower is better)
			if cfg.Thresholds.TCPLatency.Pass >= cfg.Thresholds.TCPLatency.Warn {
				t.Error("TCPLatency: Pass must be < Warn")
			}
		})
	}
}

func TestPlatformConfigStructureConsistency(t *testing.T) {
	var reference map[string]any
	var referencePlatform string
	referenceAssigned := false

	for platform, filename := range platformFileMap {
		data, err := platformFS.ReadFile(filename)
		if err != nil {
			t.Fatalf("%s: failed to read: %v", platform, err)
		}

		var raw map[string]any
		if err := yaml.Unmarshal(data, &raw); err != nil {
			t.Fatalf("%s: failed to parse: %v", platform, err)
		}

		if !referenceAssigned {
			reference = raw
			referencePlatform = string(platform)
			referenceAssigned = true
			continue
		}

		for key := range raw {
			if key == "platform" {
				continue
			}
			if _, ok := reference[key]; !ok {
				t.Errorf("%s has key %q missing from %s", platform, key, referencePlatform)
			}
		}

		for key := range reference {
			if key == "platform" {
				continue
			}
			if _, ok := raw[key]; !ok {
				t.Errorf("%s missing key %q (present in %s)", platform, key, referencePlatform)
			}
		}
	}
}

func TestGetConfig_UnknownPlatform(t *testing.T) {
	cfg, err := GetConfig(PlatformUnknown)
	if err != nil {
		t.Fatalf("GetConfig(Unknown) returned error: %v", err)
	}

	if cfg.GPU.MinDriverVersion == "" {
		t.Error("Unknown platform should fall back to defaults with min_driver_version")
	}
}

func TestResourceConfig_RequestsAndLimits(t *testing.T) {
	cfg, err := GetConfig(PlatformOCP)
	if err != nil {
		t.Fatalf("GetConfig(OCP) returned error: %v", err)
	}

	// Agent: requests set, no limits by default
	if cfg.Agent.Requests["cpu"] != "500m" {
		t.Errorf("Agent cpu request = %q, want 500m", cfg.Agent.Requests["cpu"])
	}
	if cfg.Agent.Requests["memory"] != "512Mi" {
		t.Errorf("Agent memory request = %q, want 512Mi", cfg.Agent.Requests["memory"])
	}

	// Jobs: requests set, limits empty by default
	if cfg.Jobs.Requests["cpu"] != "500m" {
		t.Errorf("Jobs cpu request = %q, want 500m", cfg.Jobs.Requests["cpu"])
	}

	// No GPU/RDMA in default config — those are auto-detected or manual
	if _, ok := cfg.Jobs.Requests["nvidia.com/gpu"]; ok {
		t.Error("Jobs should not have nvidia.com/gpu in default config")
	}
	if _, ok := cfg.Jobs.Requests["nvidia.com/roce"]; ok {
		t.Error("Jobs should not have nvidia.com/roce in default config")
	}
}

func TestResourceConfig_Annotations(t *testing.T) {
	cfg, err := GetConfig(PlatformAKS)
	if err != nil {
		t.Fatalf("GetConfig(AKS) returned error: %v", err)
	}

	// Annotations should be initialized (empty, not nil)
	if cfg.Agent.Annotations == nil {
		t.Error("Agent.Annotations should not be nil")
	}
	if cfg.Jobs.Annotations == nil {
		t.Error("Jobs.Annotations should not be nil")
	}
}

func TestResourceConfig_WithRDMA(t *testing.T) {
	// Simulate a config with RDMA resources
	rc := ResourceConfig{
		Requests: map[string]string{
			"cpu":             "500m",
			"memory":          "512Mi",
			"nvidia.com/roce": "1",
		},
		Limits: map[string]string{
			"nvidia.com/roce": "1",
		},
	}

	// RDMA in requests
	if rc.Requests["nvidia.com/roce"] != "1" {
		t.Error("RDMA should be in requests")
	}

	// RDMA in limits (must match requests for device resources)
	if rc.Limits["nvidia.com/roce"] != "1" {
		t.Error("RDMA should be in limits")
	}

	// CPU only in requests, not limits
	if _, ok := rc.Limits["cpu"]; ok {
		t.Error("CPU should not be in limits")
	}
}

func TestValidate_ThresholdSemantics(t *testing.T) {
	tests := []struct {
		name      string
		modify    func(*PlatformConfig)
		wantError bool
	}{
		{
			name:      "valid config",
			modify:    func(c *PlatformConfig) {},
			wantError: false,
		},
		{
			name: "invalid tcp bandwidth: pass <= warn",
			modify: func(c *PlatformConfig) {
				c.Thresholds.TCPBandwidth.Pass = 5.0
				c.Thresholds.TCPBandwidth.Warn = 10.0
			},
			wantError: true,
		},
		{
			name: "invalid tcp latency: pass >= warn",
			modify: func(c *PlatformConfig) {
				c.Thresholds.TCPLatency.Pass = 2.0
				c.Thresholds.TCPLatency.Warn = 1.0
			},
			wantError: true,
		},
		{
			name: "invalid rdma bandwidth pd: pass <= warn",
			modify: func(c *PlatformConfig) {
				c.Thresholds.RDMABandwidthPD.Pass = 100.0
				c.Thresholds.RDMABandwidthPD.Warn = 200.0
			},
			wantError: true,
		},
		{
			name: "invalid rdma bandwidth wep: pass <= warn",
			modify: func(c *PlatformConfig) {
				c.Thresholds.RDMABandwidthWEP.Pass = 200.0
				c.Thresholds.RDMABandwidthWEP.Warn = 300.0
			},
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := GetConfig(PlatformOCP)
			if err != nil {
				t.Fatalf("GetConfig failed: %v", err)
			}

			tt.modify(&cfg)

			err = cfg.Validate()
			if (err != nil) != tt.wantError {
				t.Errorf("Validate() error = %v, wantError = %v", err, tt.wantError)
			}
		})
	}
}
