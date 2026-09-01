package rdma

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/config"
)

// ResolveRDMAType returns the configured RDMA type or infers one consistent
// type from the GPU-paired NICs across the supplied node topologies
func ResolveRDMAType(configuredType string, topoMap map[string]*checks.NodeTopology) (config.RDMAType, error) {
	if rt := config.RDMAType(configuredType); rt == config.RDMATypeIB || rt == config.RDMATypeRoCE || rt == config.RDMATypeSRD {
		return rt, nil
	}

	// Slim topology pairs do not carry LinkLayer, so resolve each paired device
	// against the fully deserialized NIC list
	linkLayers := make(map[checks.LinkLayer]bool)
	for _, topo := range topoMap {
		nicByDev := make(map[string]checks.LinkLayer, len(topo.NICList))
		for _, nic := range topo.NICList {
			nicByDev[nic.Dev] = nic.LinkLayer
		}
		for _, pair := range topo.Pairs {
			ll, ok := nicByDev[pair.NIC.Dev]
			if !ok {
				return "", fmt.Errorf("paired NIC %q not found in NICList for topology inference", pair.NIC.Dev)
			}
			linkLayers[ll] = true
		}
	}

	switch {
	case len(linkLayers) == 0:
		return "", fmt.Errorf("no NIC link layer data in topology")
	case len(linkLayers) > 1:
		return "", fmt.Errorf("mixed link layer types detected in GPU-paired NICs; set jobs.rdma_type explicitly")
	case linkLayers[checks.LinkLayerEthernet]:
		return config.RDMATypeRoCE, nil
	case linkLayers[checks.LinkLayerInfiniBand]:
		return config.RDMATypeIB, nil
	case linkLayers[checks.LinkLayerSRD]:
		return config.RDMATypeSRD, nil
	default:
		return "", fmt.Errorf("unknown link layer type in topology")
	}
}

// listVerbsDevices returns all InfiniBand verbs device names visible to
// the container. It tries ibv_devices first, falling back to reading
// /sys/class/infiniband entries. Returns os.ErrNotExist when the sysfs
// directory is absent (no RDMA subsystem at all).
func listVerbsDevices(ctx context.Context) ([]string, error) {
	output, err := exec.CommandContext(ctx, "ibv_devices").Output()
	if err == nil {
		return parseIBVDevices(string(output)), nil
	}

	entries, sysErr := os.ReadDir("/sys/class/infiniband")
	if sysErr != nil {
		if errors.Is(sysErr, os.ErrNotExist) {
			return nil, sysErr
		}
		return nil, fmt.Errorf("ibv_devices failed: %v; sysfs fallback also failed: %v", err, sysErr)
	}
	var devices []string
	for _, e := range entries {
		devices = append(devices, e.Name())
	}
	return devices, nil
}

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

	if _, err := os.Stat("/dev/infiniband"); err != nil {
		r.Status = checks.StatusFail
		if os.IsNotExist(err) {
			r.Message = "/dev/infiniband not found"
			r.Remediation = "Enable RDMA networking on node pool"
		} else {
			r.Message = fmt.Sprintf("Cannot access /dev/infiniband: %v", err)
			r.Remediation = "Check the device mount and container permissions"
		}
		return r
	}

	verbsDevices, err := listVerbsDevices(ctx)
	if err != nil {
		r.Status = checks.StatusFail
		r.Message = fmt.Sprintf("Failed to enumerate RDMA devices: %v", err)
		r.Remediation = "Check RDMA device plugin and network operator installation"
		return r
	}
	if len(verbsDevices) == 0 {
		r.Status = checks.StatusFail
		r.Message = "No RDMA devices found via ibv_devices or sysfs"
		r.Remediation = "Check RDMA device plugin and network operator installation"
		return r
	}

	var rdmaCapable, verbsOnly []string
	for _, dev := range verbsDevices {
		if hasRDMACapability(ctx, dev) {
			rdmaCapable = append(rdmaCapable, dev)
		} else {
			verbsOnly = append(verbsOnly, dev)
		}
	}

	r.Details = map[string]any{
		"verbs_devices": verbsDevices,
		"rdma_devices":  rdmaCapable,
		"verbs_only":    verbsOnly,
	}

	if len(rdmaCapable) > 0 {
		r.Status = checks.StatusPass
		r.Message = fmt.Sprintf("%d RDMA-capable device(s): %s", len(rdmaCapable), strings.Join(rdmaCapable, ", "))
		if len(verbsOnly) > 0 {
			r.Message += fmt.Sprintf(" (%d other verbs devices: %s)", len(verbsOnly), strings.Join(verbsOnly, ", "))
		}
	} else {
		r.Status = checks.StatusWarn
		r.Message = fmt.Sprintf("%d verbs device(s) found (%s) but 0 are RDMA-capable (no GIDs)",
			len(verbsOnly), strings.Join(verbsOnly, ", "))
	}
	return r
}

const zeroGID = "0000:0000:0000:0000:0000:0000:0000:0000"

// hasRDMACapability determines whether a /sys/class/infiniband device is a
// genuine RDMA-capable NIC (InfiniBand or RoCE) rather than a phantom verbs
// device (e.g. Azure Accelerated Networking SR-IOV VFs that expose mlx5_ib
// entries but have no actual RDMA transport).
//
// Detection strategy:
//  0. EFA PCI vendor/device (AWS) → capable; EFA ports may have empty GIDs.
//     Note: EFA RDMA-capable ≠ GPUDirect RDMA. Some EKS SKUs (e.g. p5.4xlarge)
//     have EFA for host fabric but not GPUDirect; that affects --use_cuda
//     bandwidth jobs, not whether the NIC counts here.
//  1. InfiniBand link_layer → immediately capable (IB devices are always
//     RDMA-capable regardless of link state or GID table).
//  2. Ethernet link_layer (RoCE) → check gid_attrs/ndevs/ for a netdev
//     association. This persists even when the link is administratively down,
//     so it avoids false negatives for capable-but-down RoCE NICs. Azure AN
//     phantom VFs have an empty ndevs directory because the hypervisor blocks
//     RoCE — the kernel never creates the netdev-to-GID association.
//  3. GID scan fallback → for older kernels that may not expose gid_attrs,
//     scan all GID entries for any non-zero value.
func hasRDMACapability(_ context.Context, dev string) bool {
	if IsEFADevice(dev) {
		return true
	}

	portsPath := filepath.Join("/sys/class/infiniband", dev, "ports")
	ports, err := os.ReadDir(portsPath)
	if err != nil {
		return false
	}

	for _, port := range ports {
		if !port.IsDir() {
			continue
		}
		portPath := filepath.Join(portsPath, port.Name())

		// Step 1: InfiniBand link layer — always RDMA-capable.
		linkLayer := ""
		if llData, err := os.ReadFile(filepath.Join(portPath, "link_layer")); err == nil {
			linkLayer = strings.TrimSpace(string(llData))
		}
		if linkLayer == "InfiniBand" {
			return true
		}

		// Step 2: For Ethernet (RoCE), check if the kernel has associated a
		// netdev with this port. Real RoCE NICs have ndevs entries even when
		// the link is down; Azure AN phantom VFs have an empty ndevs dir.
		ndevsPath := filepath.Join(portPath, "gid_attrs", "ndevs")
		if ndevs, err := os.ReadDir(ndevsPath); err == nil {
			for _, ndev := range ndevs {
				data, err := os.ReadFile(filepath.Join(ndevsPath, ndev.Name()))
				if err == nil && strings.TrimSpace(string(data)) != "" {
					return true
				}
			}
		}

		// Step 3: GID scan fallback for kernels without gid_attrs.
		gidsPath := filepath.Join(portPath, "gids")
		if gids, err := os.ReadDir(gidsPath); err == nil {
			for _, gidFile := range gids {
				data, err := os.ReadFile(filepath.Join(gidsPath, gidFile.Name()))
				if err != nil {
					continue
				}
				if gid := strings.TrimSpace(string(data)); gid != "" && gid != zeroGID {
					return true
				}
			}
		}
	}

	return false
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
