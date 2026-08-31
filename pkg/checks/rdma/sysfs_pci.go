package rdma

import (
	"os"
	"path/filepath"
	"strings"
)

// pciIDsForRDMADevice reads PCI vendor and device IDs from sysfs for an
// InfiniBand verbs device (IB, RoCE, EFA/SRD, etc.).
func pciIDsForRDMADevice(dev string) (vendor, device string, ok bool) {
	base := filepath.Join(sysfsNICRoot, dev, "device")
	vendorData, err := os.ReadFile(filepath.Join(base, "vendor"))
	if err != nil {
		return "", "", false
	}
	deviceData, err := os.ReadFile(filepath.Join(base, "device"))
	if err != nil {
		return "", "", false
	}
	return normalizePCIID(string(vendorData)), normalizePCIID(string(deviceData)), true
}

func normalizePCIID(id string) string {
	id = strings.TrimSpace(strings.ToLower(id))
	if !strings.HasPrefix(id, "0x") {
		id = "0x" + id
	}
	return id
}
