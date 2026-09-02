package rdma

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
)

// sysfsNICRoot is the sysfs infiniband class path (overridable in tests).
var sysfsNICRoot = "/sys/class/infiniband"

// listNICStatusFromSysfs enumerates RDMA NIC ports from sysfs and returns
// parsed link state. Used for EFA/SRD where ibstat returns no output; the
// same sysfs fields could replace ibstat for IB/RoCE in the future.
// When devices is non-empty, only those HCAs are queried (matching ibv_devices
// enumeration); otherwise all entries under sysfsNICRoot are scanned.
func listNICStatusFromSysfs(_ context.Context, devices []string) ([]NICStatusInfo, error) {
	if len(devices) == 0 {
		entries, err := os.ReadDir(sysfsNICRoot)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if entry.Name() != "." && entry.Name() != ".." {
				devices = append(devices, entry.Name())
			}
		}
	}

	var nics []NICStatusInfo
	for _, dev := range devices {
		portsPath := filepath.Join(sysfsNICRoot, dev, "ports")
		for _, portNum := range listSysfsPortNums(portsPath) {
			portPath := filepath.Join(portsPath, portNum)
			nic, ok := readPortStatus(dev, portNum, portPath)
			if !ok {
				continue
			}
			nics = append(nics, nic)
		}
	}
	if len(nics) == 0 {
		return nil, fmt.Errorf("no RDMA NIC ports found in sysfs")
	}
	return nics, nil
}

// listSysfsPortNums returns port directory names under an HCA ports path.
// Falls back to port "1" when ReadDir is empty but the default port exists
// (common for single-port EFA and mlx5 devices).
func listSysfsPortNums(portsPath string) []string {
	ports, err := os.ReadDir(portsPath)
	if err != nil {
		if _, statErr := os.Stat(filepath.Join(portsPath, "1")); statErr == nil {
			return []string{"1"}
		}
		return nil
	}

	var nums []string
	for _, port := range ports {
		name := port.Name()
		if name == "." || name == ".." {
			continue
		}
		nums = append(nums, name)
	}
	if len(nums) == 0 {
		if _, statErr := os.Stat(filepath.Join(portsPath, "1")); statErr == nil {
			return []string{"1"}
		}
	}
	return nums
}

func readPortStatus(dev, portNum, portPath string) (NICStatusInfo, bool) {
	stateRaw, err := readSysfsPortFile(filepath.Join(portPath, "state"))
	if err != nil {
		return NICStatusInfo{}, false
	}
	physRaw, _ := readSysfsPortFile(filepath.Join(portPath, "phys_state"))

	state := parseSysfsState(stateRaw)
	if state == "Active" && !isPortActive(stateRaw, physRaw) {
		state = "Down"
	}

	nic := NICStatusInfo{
		Name:  portStatusName(dev, portNum),
		State: state,
	}

	if rateRaw, err := readSysfsPortFile(filepath.Join(portPath, "rate")); err == nil {
		nic.Rate = parseSysfsRate(rateRaw)
	}
	if llRaw, err := readSysfsPortFile(filepath.Join(portPath, "link_layer")); err == nil {
		nic.LinkLayer = string(resolveLinkLayer(dev, llRaw))
	}
	return nic, true
}

// resolveLinkLayer normalizes sysfs ports/1/link_layer for dev. EFA NICs often
// report "Unknown"; map those to SRD so topology and pingmesh match status checks.
func resolveLinkLayer(dev, rawSysfs string) checks.LinkLayer {
	ll := checks.LinkLayer(strings.TrimSpace(rawSysfs))
	if ll == checks.LinkLayerUnknown && IsEFADevice(dev) {
		return checks.LinkLayerSRD
	}
	return ll
}

func readSysfsPortFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func portStatusName(dev, portNum string) string {
	if portNum == "1" {
		return dev
	}
	return fmt.Sprintf("%s/port%s", dev, portNum)
}

// parseSysfsState converts sysfs state (e.g. "4: ACTIVE") to ibstat-style names.
func parseSysfsState(raw string) string {
	raw = strings.TrimSpace(raw)
	if idx := strings.Index(raw, ":"); idx >= 0 {
		raw = strings.TrimSpace(raw[idx+1:])
	}
	switch strings.ToUpper(raw) {
	case "ACTIVE":
		return "Active"
	case "DOWN":
		return "Down"
	default:
		if raw == "" {
			return "Unknown"
		}
		return raw
	}
}

// isPortActive returns true when the port is ACTIVE and physically LinkUp.
func isPortActive(stateRaw, physRaw string) bool {
	state := strings.ToUpper(strings.TrimSpace(stateRaw))
	if !strings.Contains(state, "ACTIVE") {
		return false
	}
	phys := strings.TrimSpace(physRaw)
	if phys == "" {
		return true
	}
	return strings.Contains(phys, "LinkUp")
}

// parseSysfsRate extracts the leading speed from sysfs rate strings such as
// "100 Gb/sec (4X EDR)".
func parseSysfsRate(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	fields := strings.Fields(raw)
	if len(fields) > 0 {
		return fields[0]
	}
	return raw
}
