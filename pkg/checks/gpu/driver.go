package gpu

import (
	"context"
	"encoding/csv"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
)

// DriverCheck validates the NVIDIA GPU driver version.
type DriverCheck struct {
	nodeName   string
	minVersion string
}

func NewDriverCheck(nodeName, minVersion string) *DriverCheck {
	return &DriverCheck{
		nodeName:   nodeName,
		minVersion: minVersion,
	}
}

func (c *DriverCheck) Name() string     { return "gpu_driver_version" }
func (c *DriverCheck) Category() string { return "gpu_hardware" }

func (c *DriverCheck) Run(ctx context.Context) checks.Result {
	r := checks.Result{
		Node:     c.nodeName,
		Category: c.Category(),
		Name:     c.Name(),
	}

	output, err := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=driver_version,cuda_version,gpu_name,memory.total",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		r.Status = checks.StatusFail
		r.Message = fmt.Sprintf("nvidia-smi failed: %v", err)
		r.Remediation = "Ensure NVIDIA GPU driver is installed and nvidia-smi is available"
		return r
	}

	info, err := parseDriverOutput(string(output))
	if err != nil {
		r.Status = checks.StatusFail
		r.Message = err.Error()
		return r
	}

	r.Details = map[string]any{
		"driver_version":     info.DriverVersion,
		"cuda_version":       info.CUDAVersion,
		"gpu_product":        info.GPUName,
		"memory_total":       info.MemoryTotal,
		"gpu_count":          info.GPUCount,
		"min_driver_version": c.minVersion,
	}

	// Compare driver version against minimum
	if c.minVersion != "" && compareVersions(info.DriverVersion, c.minVersion) < 0 {
		r.Status = checks.StatusFail
		r.Message = fmt.Sprintf("Driver version %s is below minimum %s", info.DriverVersion, c.minVersion)
		r.Remediation = fmt.Sprintf("Upgrade NVIDIA GPU driver to >= %s", c.minVersion)
		return r
	}

	r.Status = checks.StatusPass
	r.Message = fmt.Sprintf("NVIDIA driver: %s, CUDA: %s, GPU: %s (%s MiB), %d GPU(s)",
		info.DriverVersion, info.CUDAVersion, info.GPUName, info.MemoryTotal, info.GPUCount)

	return r
}

// driverInfo holds parsed nvidia-smi driver query output.
type driverInfo struct {
	DriverVersion string
	CUDAVersion   string
	GPUName       string
	MemoryTotal   string
	GPUCount      int
}

// parseDriverOutput parses nvidia-smi CSV output for driver_version,cuda_version,gpu_name,memory.total.
func parseDriverOutput(output string) (*driverInfo, error) {
	reader := csv.NewReader(strings.NewReader(strings.TrimSpace(output)))
	records, err := reader.ReadAll()
	if err != nil || len(records) == 0 {
		return nil, fmt.Errorf("failed to parse nvidia-smi output")
	}

	fields := records[0]
	if len(fields) < 4 {
		return nil, fmt.Errorf("unexpected nvidia-smi output format")
	}

	return &driverInfo{
		DriverVersion: strings.TrimSpace(fields[0]),
		CUDAVersion:   strings.TrimSpace(fields[1]),
		GPUName:       strings.TrimSpace(fields[2]),
		MemoryTotal:   strings.TrimSpace(fields[3]),
		GPUCount:      len(records),
	}, nil
}

// compareVersions compares two dot-separated version strings numerically.
// Returns -1 if a < b, 0 if a == b, 1 if a > b.
func compareVersions(a, b string) int {
	aParts := strings.Split(a, ".")
	bParts := strings.Split(b, ".")

	maxLen := len(aParts)
	if len(bParts) > maxLen {
		maxLen = len(bParts)
	}

	for i := 0; i < maxLen; i++ {
		aNum := 0
		bNum := 0
		if i < len(aParts) {
			aNum, _ = strconv.Atoi(aParts[i])
		}
		if i < len(bParts) {
			bNum, _ = strconv.Atoi(bParts[i])
		}
		if aNum < bNum {
			return -1
		}
		if aNum > bNum {
			return 1
		}
	}
	return 0
}
