package rdma

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
)

// DevicesCheck validates RDMA device presence and accessibility.
type DevicesCheck struct {
	nodeName string
}

func NewDevicesCheck(nodeName string) *DevicesCheck {
	return &DevicesCheck{nodeName: nodeName}
}

func (c *DevicesCheck) Name() string     { return "rdma_devices_detected" }
func (c *DevicesCheck) Category() string { return "networking_rdma" }

func (c *DevicesCheck) Run(ctx context.Context) checks.Result {
	r := checks.Result{
		Node:     c.nodeName,
		Category: c.Category(),
		Name:     c.Name(),
	}

	// Check /dev/infiniband exists
	if _, err := os.Stat("/dev/infiniband"); os.IsNotExist(err) {
		r.Status = checks.StatusFail
		r.Message = "/dev/infiniband not found"
		r.Remediation = "Enable accelerated networking / InfiniBand on node pool"
		return r
	}

	// Use ibv_devices for vendor-neutral RDMA device discovery
	output, err := exec.CommandContext(ctx, "ibv_devices").Output()
	if err != nil {
		r.Status = checks.StatusFail
		r.Message = fmt.Sprintf("ibv_devices failed: %v", err)
		r.Remediation = "Install rdma-core / libibverbs-utils"
		return r
	}

	devices := parseIBVDevices(string(output))
	if len(devices) == 0 {
		r.Status = checks.StatusFail
		r.Message = "No RDMA devices found via ibv_devices"
		r.Remediation = "Check RDMA device plugin and network operator installation"
		return r
	}

	r.Status = checks.StatusPass
	r.Message = fmt.Sprintf("%d RDMA device(s) found: %s", len(devices), strings.Join(devices, ", "))
	r.Details = map[string]any{"devices": devices}
	return r
}

func parseIBVDevices(output string) []string {
	var devices []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "device") || strings.Contains(line, "---") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) > 0 {
			devices = append(devices, fields[0])
		}
	}
	return devices
}
