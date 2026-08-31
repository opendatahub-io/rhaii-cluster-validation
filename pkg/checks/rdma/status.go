package rdma

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/config"
)

// StatusCheck validates RDMA NIC link state and speed.
//
// IB and RoCE use ibstat (existing path). EFA/SRD (rdma_type=srd) uses sysfs
// because ibstat returns no output for EFA devices on AWS p5 nodes. The same
// sysfs port fields can be used for ib/roce in the future.
type StatusCheck struct {
	nodeName string
	rdmaType config.RDMAType
}

func NewStatusCheck(nodeName string, rdmaType config.RDMAType) *StatusCheck {
	return &StatusCheck{nodeName: nodeName, rdmaType: rdmaType}
}

func (c *StatusCheck) Name() string     { return "rdma_nic_status" }
func (c *StatusCheck) Category() string { return "networking_rdma" }

func (c *StatusCheck) Run(ctx context.Context) checks.Result {
	r := checks.Result{
		Node:     c.nodeName,
		Category: c.Category(),
		Name:     c.Name(),
	}

	verbsDevices, err := listVerbsDevices(ctx)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			r.Status = checks.StatusSkip
			r.Message = "No RDMA devices present (/sys/class/infiniband not found)"
			return r
		}
		r.Status = checks.StatusFail
		r.Message = fmt.Sprintf("Failed to enumerate RDMA devices: %v", err)
		return r
	}
	hasRDMA := false
	for _, dev := range verbsDevices {
		if hasRDMACapability(ctx, dev) {
			hasRDMA = true
			break
		}
	}
	if !hasRDMA {
		r.Status = checks.StatusSkip
		r.Message = "No RDMA-capable devices found (verbs-only or no GIDs), skipping link status check"
		return r
	}

	if c.rdmaType == config.RDMATypeSRD {
		return c.runEFAStatus(ctx, r, verbsDevices)
	}
	return c.runIBStatStatus(ctx, r)
}

// runEFAStatus reads link state from sysfs for EFA devices. ibstat does not
// list EFA NICs on AWS p5 nodes even when ibv_devices shows them.
func (c *StatusCheck) runEFAStatus(ctx context.Context, r checks.Result, verbsDevices []string) checks.Result {
	var efaDevs []string
	for _, dev := range verbsDevices {
		if hasRDMACapability(ctx, dev) && IsEFADevice(dev) {
			efaDevs = append(efaDevs, dev)
		}
	}
	if len(efaDevs) == 0 {
		r.Status = checks.StatusWarn
		r.Message = "No EFA devices found for rdma_type=srd link status check"
		return r
	}

	nics, err := listNICStatusFromSysfs(ctx, efaDevs)
	if err != nil {
		r.Status = checks.StatusFail
		r.Message = fmt.Sprintf("sysfs NIC status read failed: %v", err)
		r.Remediation = "Check RDMA driver and device plugin installation"
		return r
	}

	r.Details = map[string]any{"nics": nics}
	return c.summarizeStatus(r, nics, "sysfs")
}

func (c *StatusCheck) runIBStatStatus(ctx context.Context, r checks.Result) checks.Result {
	output, err := exec.CommandContext(ctx, "ibstat").Output()
	if err != nil {
		r.Status = checks.StatusFail
		r.Message = fmt.Sprintf("ibstat failed: %v", err)
		r.Remediation = "Check RDMA driver and device plugin installation"
		return r
	}

	nics := parseIBStat(string(output))
	if len(nics) == 0 {
		r.Status = checks.StatusFail
		r.Message = "No RDMA NICs found via ibstat"
		return r
	}

	r.Details = map[string]any{"nics": nics}
	return c.summarizeStatus(r, nics, "ibstat")
}

func (c *StatusCheck) summarizeStatus(r checks.Result, nics []NICStatusInfo, source string) checks.Result {
	var targetActive, targetDown, otherActive, otherDown []string
	for _, nic := range nics {
		isTarget := c.isTargetNIC(nic)
		if isTarget {
			desc := fmt.Sprintf("%s (%s Gbps)", nic.Name, nic.Rate)
			if nic.State == "Active" {
				targetActive = append(targetActive, desc)
			} else {
				targetDown = append(targetDown, nic.Name)
			}
		} else {
			if nic.State == "Active" {
				otherActive = append(otherActive, nic.Name)
			} else {
				otherDown = append(otherDown, nic.Name)
			}
		}
	}

	otherNote := ""
	otherTotal := len(otherActive) + len(otherDown)
	if otherTotal > 0 {
		otherNote = fmt.Sprintf(" (other NICs: %d up, %d down)", len(otherActive), len(otherDown))
	}

	if len(targetActive) == 0 && len(targetDown) == 0 {
		r.Status = checks.StatusWarn
		r.Message = fmt.Sprintf("No NICs matching rdma_type=%q found via %s", c.rdmaType, source) + otherNote
		return r
	}

	if len(targetDown) > 0 && len(targetActive) == 0 {
		r.Status = checks.StatusFail
		r.Message = fmt.Sprintf("All %s NIC(s) down: %s", c.rdmaTypeLabel(), strings.Join(targetDown, ", ")) + otherNote
		r.Remediation = "Check NIC, cable, and switch configuration"
	} else if len(targetDown) > 0 {
		r.Status = checks.StatusWarn
		r.Message = fmt.Sprintf("%d/%d %s NIC(s) down: %s; active: %s",
			len(targetDown), len(targetDown)+len(targetActive), c.rdmaTypeLabel(),
			strings.Join(targetDown, ", "), strings.Join(targetActive, ", ")) + otherNote
	} else {
		r.Status = checks.StatusPass
		r.Message = fmt.Sprintf("%d active %s NIC(s): %s", len(targetActive), c.rdmaTypeLabel(), strings.Join(targetActive, ", ")) + otherNote
	}
	return r
}

func (c *StatusCheck) rdmaTypeLabel() string {
	if c.rdmaType == "" {
		return "RDMA"
	}
	return string(c.rdmaType)
}

// isTargetNIC returns true if the NIC matches the configured rdma_type.
// If rdma_type is empty, all NICs are targets.
func (c *StatusCheck) isTargetNIC(nic NICStatusInfo) bool {
	if c.rdmaType == "" {
		return true
	}
	if c.rdmaType == config.RDMATypeIB {
		return nic.LinkLayer == string(checks.LinkLayerInfiniBand)
	}
	if c.rdmaType == config.RDMATypeRoCE {
		return nic.LinkLayer == string(checks.LinkLayerEthernet)
	}
	if c.rdmaType == config.RDMATypeSRD {
		return IsEFADevice(efaDeviceFromPortName(nic.Name))
	}
	return true
}

// efaDeviceFromPortName returns the HCA device name for IsEFADevice lookup.
// Sysfs port status uses "dev/portN" names; EFA PCI sysfs is keyed by dev.
func efaDeviceFromPortName(name string) string {
	if idx := strings.Index(name, "/port"); idx >= 0 {
		return name[:idx]
	}
	return name
}

// NICStatusInfo holds parsed NIC port information from ibstat or sysfs.
type NICStatusInfo struct {
	Name      string `json:"name"`
	State     string `json:"state"`
	Rate      string `json:"rate"`
	LinkLayer string `json:"link_layer"`
}

var (
	ibstatNameRe  = regexp.MustCompile(`CA '([^']+)'`)
	ibstatPortRe  = regexp.MustCompile(`Port (\d+):`)
	ibstatStateRe = regexp.MustCompile(`State:\s+(\S+)`)
	ibstatRateRe  = regexp.MustCompile(`Rate:\s+(\S+)`)
	ibstatLLRe    = regexp.MustCompile(`Link layer:\s+(\S+)`)
)

func parseIBStat(output string) []NICStatusInfo {
	var nics []NICStatusInfo

	sections := strings.Split(output, "CA '")
	for _, section := range sections[1:] {
		caName := ""
		if m := ibstatNameRe.FindStringSubmatch("CA '" + section); len(m) > 1 {
			caName = m[1]
		}
		if caName == "" {
			continue
		}

		portSections := ibstatPortRe.Split(section, -1)
		portNumbers := ibstatPortRe.FindAllStringSubmatch(section, -1)

		if len(portSections) <= 1 {
			nic := NICStatusInfo{Name: caName}
			if m := ibstatStateRe.FindStringSubmatch(section); len(m) > 1 {
				nic.State = m[1]
			}
			if m := ibstatRateRe.FindStringSubmatch(section); len(m) > 1 {
				nic.Rate = m[1]
			}
			if m := ibstatLLRe.FindStringSubmatch(section); len(m) > 1 {
				nic.LinkLayer = m[1]
			}
			nics = append(nics, nic)
			continue
		}

		for i, portSection := range portSections[1:] {
			portNum := ""
			if i < len(portNumbers) {
				portNum = portNumbers[i][1]
			}

			nic := NICStatusInfo{Name: fmt.Sprintf("%s/port%s", caName, portNum)}
			if m := ibstatStateRe.FindStringSubmatch(portSection); len(m) > 1 {
				nic.State = m[1]
			}
			if m := ibstatRateRe.FindStringSubmatch(portSection); len(m) > 1 {
				nic.Rate = m[1]
			}
			if m := ibstatLLRe.FindStringSubmatch(portSection); len(m) > 1 {
				nic.LinkLayer = m[1]
			}
			nics = append(nics, nic)
		}
	}

	return nics
}
