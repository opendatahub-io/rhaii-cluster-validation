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

	GPU              GPUConfig          `yaml:"gpu" json:"gpu"`
	RDMA             RDMAConfig         `yaml:"rdma" json:"rdma"`
	Thresholds       ThresholdConfig    `yaml:"thresholds" json:"thresholds"`
	PodConfiguration PodConfiguration   `yaml:"podConfiguration" json:"podConfiguration"`
}

// PodConfiguration holds pod-level settings applied to agent DaemonSet and job pods.
// Customizable per platform — different GPU types and clouds may need different resources.
type PodConfiguration struct {
	Annotations      map[string]string `yaml:"annotations,omitempty" json:"annotations,omitempty"`
	ResourceRequests map[string]string `yaml:"resourceRequests,omitempty" json:"resourceRequests,omitempty"`
	ResourceLimits   map[string]string `yaml:"resourceLimits,omitempty" json:"resourceLimits,omitempty"`
	Privileged       *bool             `yaml:"privileged,omitempty" json:"privileged,omitempty"`
}

// GPUConfig holds GPU-related platform defaults.
type GPUConfig struct {
	SupportedTypes   []string `yaml:"supported_types" json:"supported_types"`
	DevicePlugin     string   `yaml:"device_plugin" json:"device_plugin"`
	TaintKey         string   `yaml:"taint_key" json:"taint_key"`
	TaintValue       string   `yaml:"taint_value" json:"taint_value"`
	MinDriverVersion string   `yaml:"min_driver_version" json:"min_driver_version"`
	NvidiaSmiPath    string   `yaml:"nvidia_smi_path" json:"nvidia_smi_path"`
	NvidiaLibPath    string   `yaml:"nvidia_lib_path" json:"nvidia_lib_path"`
}

// RDMAConfig holds RDMA-related platform defaults.
type RDMAConfig struct {
	DevicePlugin   string `yaml:"device_plugin" json:"device_plugin"`
	NICPrefix      string `yaml:"nic_prefix" json:"nic_prefix"`
	ExpectedPerGPU int    `yaml:"expected_devices_per_gpu" json:"expected_devices_per_gpu"`
}

// ThresholdConfig holds network performance thresholds.
type ThresholdConfig struct {
	TCPBandwidth     BandwidthThreshold `yaml:"tcp_bandwidth_gbps" json:"tcp_bandwidth_gbps"`
	RDMABandwidthPD  BandwidthThreshold `yaml:"rdma_bandwidth_pd_gbps" json:"rdma_bandwidth_pd_gbps"`
	RDMABandwidthWEP BandwidthThreshold `yaml:"rdma_bandwidth_wep_gbps" json:"rdma_bandwidth_wep_gbps"`
	TCPLatencyMs     LatencyThreshold   `yaml:"tcp_latency_ms" json:"tcp_latency_ms"`
}

// BandwidthThreshold defines pass/warn/fail thresholds for bandwidth.
type BandwidthThreshold struct {
	Pass float64 `yaml:"pass" json:"pass"`
	Warn float64 `yaml:"warn" json:"warn"`
	Fail float64 `yaml:"fail" json:"fail"`
}

// LatencyThreshold defines pass/warn/fail thresholds for latency.
type LatencyThreshold struct {
	Pass float64 `yaml:"pass" json:"pass"`
	Warn float64 `yaml:"warn" json:"warn"`
	Fail float64 `yaml:"fail" json:"fail"`
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
