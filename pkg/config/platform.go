package config

import (
	"embed"
	"fmt"

	"gopkg.in/yaml.v3"
)

//go:embed platforms/*.yaml
var platformFS embed.FS

// Platform represents a detected cloud platform.
type Platform string

const (
	PlatformAKS       Platform = "AKS"
	PlatformEKS       Platform = "EKS"
	PlatformCoreWeave Platform = "CoreWeave"
	PlatformOCP       Platform = "OCP"
	PlatformUnknown   Platform = "Unknown"
)

// PlatformConfig holds platform-specific defaults for validation checks.
type PlatformConfig struct {
	Platform Platform `yaml:"platform" json:"platform"`

	Agent      ResourceConfig  `yaml:"agent" json:"agent"`
	Jobs       ResourceConfig  `yaml:"jobs" json:"jobs"`
	GPU        GPUConfig       `yaml:"gpu" json:"gpu"`
	CRDs       CRDConfig       `yaml:"crds" json:"crds"`
	Operators  OperatorConfig  `yaml:"operators" json:"operators"`
	Thresholds ThresholdConfig `yaml:"thresholds" json:"thresholds"`
}

// CRDConfig holds minimum version requirements for required CRDs.
type CRDConfig struct {
	// Map of CRD name to minimum required API version.
	// Empty string means any version is accepted.
	MinAPIVersions map[string]string `yaml:"min_api_versions,omitempty" json:"min_api_versions,omitempty"`
	// Map of CRD name to minimum required release version (semver).
	// Empty string means report only, don't enforce.
	MinReleaseVersions map[string]string `yaml:"min_release_versions,omitempty" json:"min_release_versions,omitempty"`
}

// OperatorConfig holds platform-specific namespace overrides for operator health checks.
type OperatorConfig struct {
	// Map of operator name (e.g. "cert-manager", "istio", "lws") to namespaces to check.
	Namespaces map[string][]string `yaml:"namespaces,omitempty" json:"namespaces,omitempty"`
}

// ResourceConfig holds resource requests, limits, and annotations for pods.
type ResourceConfig struct {
	Requests       map[string]string `yaml:"requests,omitempty" json:"requests,omitempty"`
	Limits         map[string]string `yaml:"limits,omitempty" json:"limits,omitempty"`
	Annotations    map[string]string `yaml:"annotations,omitempty" json:"annotations,omitempty"`
	RDMAType       string            `yaml:"rdma_type,omitempty" json:"rdma_type,omitempty"`
	RDMA           RDMAJobConfig     `yaml:"rdma,omitempty" json:"rdma,omitempty"`
	PingIterations int               `yaml:"ping_iterations,omitempty" json:"ping_iterations,omitempty"`
	PingTimeout    int               `yaml:"ping_timeout,omitempty" json:"ping_timeout,omitempty"`
	PingGIDIndex   *int              `yaml:"ping_gid_index,omitempty" json:"ping_gid_index,omitempty"` // nil = auto-discover from sysfs; 0+ = fixed index
}

// GetPingGIDIndex returns the configured GID index, or -1 for auto-discover.
func (c *ResourceConfig) GetPingGIDIndex() int {
	if c.PingGIDIndex != nil {
		return *c.PingGIDIndex
	}
	return -1
}

// RDMAType identifies the RDMA fabric type for NIC filtering.
type RDMAType string

const (
	RDMATypeIB   RDMAType = "ib"
	RDMATypeRoCE RDMAType = "roce"
	RDMATypeSRD  RDMAType = "srd"
)

// RDMAJobConfig holds ib_write_bw test parameters.
// Zero values mean "use defaults" (QPs=4, MessageSize=1MiB).
type RDMAJobConfig struct {
	QPs         int `yaml:"qps,omitempty" json:"qps,omitempty"`                   // Number of queue pairs
	MessageSize int `yaml:"message_size,omitempty" json:"message_size,omitempty"` // Message size in bytes
}

// Validate checks that user-provided config values are well-formed.
func (c PlatformConfig) Validate() error {
	if rt := RDMAType(c.Jobs.RDMAType); rt != "" && rt != RDMATypeIB && rt != RDMATypeRoCE && rt != RDMATypeSRD {
		return fmt.Errorf("invalid jobs.rdma_type %q: must be %q, %q, %q, or empty", c.Jobs.RDMAType, RDMATypeIB, RDMATypeRoCE, RDMATypeSRD)
	}
	if c.Jobs.RDMA.QPs < 0 {
		return fmt.Errorf("invalid jobs.rdma.qps %d: must be >= 0", c.Jobs.RDMA.QPs)
	}
	if c.Jobs.RDMA.MessageSize < 0 {
		return fmt.Errorf("invalid jobs.rdma.message_size %d: must be >= 0", c.Jobs.RDMA.MessageSize)
	}

	// Validate bandwidth thresholds (higher is better: Pass > Warn)
	if c.Thresholds.TCPBandwidth.Pass <= c.Thresholds.TCPBandwidth.Warn {
		return fmt.Errorf("invalid tcp_bandwidth_gbps thresholds: pass (%.1f) must be > warn (%.1f)", c.Thresholds.TCPBandwidth.Pass, c.Thresholds.TCPBandwidth.Warn)
	}
	if c.Thresholds.RDMABandwidthPD.Pass <= c.Thresholds.RDMABandwidthPD.Warn {
		return fmt.Errorf("invalid rdma_bandwidth_pd_gbps thresholds: pass (%.1f) must be > warn (%.1f)", c.Thresholds.RDMABandwidthPD.Pass, c.Thresholds.RDMABandwidthPD.Warn)
	}
	if c.Thresholds.RDMABandwidthWEP.Pass <= c.Thresholds.RDMABandwidthWEP.Warn {
		return fmt.Errorf("invalid rdma_bandwidth_wep_gbps thresholds: pass (%.1f) must be > warn (%.1f)", c.Thresholds.RDMABandwidthWEP.Pass, c.Thresholds.RDMABandwidthWEP.Warn)
	}

	// Validate latency thresholds (lower is better: Pass < Warn)
	if c.Thresholds.TCPLatency.Pass >= c.Thresholds.TCPLatency.Warn {
		return fmt.Errorf("invalid tcp_latency_ms thresholds: pass (%.2f) must be < warn (%.2f)", c.Thresholds.TCPLatency.Pass, c.Thresholds.TCPLatency.Warn)
	}

	return nil
}

// GPUVendor represents the GPU hardware vendor.
type GPUVendor string

const (
	GPUVendorNVIDIA  GPUVendor = "nvidia"
	GPUVendorAMD     GPUVendor = "amd"
	GPUVendorUnknown GPUVendor = ""
)

// GPUConfig holds GPU-related configuration.
// min_driver_version is configurable per platform. Other vendor-specific settings
// (node selector, device plugin, taint) are auto-detected from node labels at runtime.
type GPUConfig struct {
	MinDriverVersion string `yaml:"min_driver_version" json:"min_driver_version"`
}

// ThresholdConfig holds network performance thresholds.
type ThresholdConfig struct {
	TCPBandwidth     BandwidthThreshold `yaml:"tcp_bandwidth_gbps" json:"tcp_bandwidth_gbps"`
	TCPLatency       LatencyThreshold   `yaml:"tcp_latency_ms" json:"tcp_latency_ms"`
	RDMABandwidthPD  BandwidthThreshold `yaml:"rdma_bandwidth_pd_gbps" json:"rdma_bandwidth_pd_gbps"`
	RDMABandwidthWEP BandwidthThreshold `yaml:"rdma_bandwidth_wep_gbps" json:"rdma_bandwidth_wep_gbps"`
}

// BandwidthThreshold defines pass/warn thresholds for bandwidth (higher is better).
// >= Pass = PASS, >= Warn = WARN, < Warn = FAIL
type BandwidthThreshold struct {
	Pass float64 `yaml:"pass" json:"pass"`
	Warn float64 `yaml:"warn" json:"warn"`
}

// LatencyThreshold defines pass/warn thresholds for latency (lower is better).
// <= Pass = PASS, <= Warn = WARN, > Warn = FAIL
type LatencyThreshold struct {
	Pass float64 `yaml:"pass" json:"pass"`
	Warn float64 `yaml:"warn" json:"warn"`
}

// platformFileMap maps platform names to their embedded config files.
var platformFileMap = map[Platform]string{
	PlatformAKS:       "platforms/aks.yaml",
	PlatformEKS:       "platforms/eks.yaml",
	PlatformCoreWeave: "platforms/coreweave.yaml",
	PlatformOCP:       "platforms/ocp.yaml",
}

// GetConfig returns the embedded platform config for the given platform.
func GetConfig(platform Platform) (PlatformConfig, error) {
	filename, ok := platformFileMap[platform]
	if !ok {
		// Fall back to AKS defaults for unknown platforms
		filename = platformFileMap[PlatformAKS]
	}

	data, err := platformFS.ReadFile(filename)
	if err != nil {
		return PlatformConfig{}, fmt.Errorf("failed to read embedded config for %s: %w", platform, err)
	}

	var cfg PlatformConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return PlatformConfig{}, fmt.Errorf("failed to parse embedded config for %s: %w", platform, err)
	}

	return cfg, nil
}
