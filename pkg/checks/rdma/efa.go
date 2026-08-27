package rdma

const efaVendorID = "0x1d0f"

// efaDeviceIDs lists Amazon EFA PCI device IDs (0xefa0–0xefa4).
var efaDeviceIDs = map[string]bool{
	"0xefa0": true,
	"0xefa1": true,
	"0xefa2": true,
	"0xefa3": true,
	"0xefa4": true,
}

// IsEFADevice returns true when dev is an AWS EFA NIC identified by PCI
// vendor 0x1d0f and device 0xefa0–0xefa4. Device names (e.g. rdmap79s0) are
// not used for identification.
func IsEFADevice(dev string) bool {
	vendor, device, ok := pciIDsForRDMADevice(dev)
	if !ok {
		return false
	}
	return isEFAPCI(vendor, device)
}

func isEFAPCI(vendor, device string) bool {
	return vendor == efaVendorID && efaDeviceIDs[device]
}
