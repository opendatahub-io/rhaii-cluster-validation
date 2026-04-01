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

	// Enumerate verbs devices using the same logic as DevicesCheck, then
	// check if any are genuinely RDMA-capable before running ibstat.
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

	// Summarize non-target NICs (different link layer than rdma_type).
	// Shown in all cases so the user can spot rdma_type misconfiguration.
	otherNote := ""
	otherTotal := len(otherActive) + len(otherDown)
	if otherTotal > 0 {
		otherNote = fmt.Sprintf(" (other %s NICs: %d up, %d down)", c.otherRDMATypeLabel(), len(otherActive), len(otherDown))
	}

	// RDMA-capable devices exist (passed GID check above) but none match
	// the configured rdma_type — likely a config mismatch, not HW failure.
	if len(targetActive) == 0 && len(targetDown) == 0 {
		r.Status = checks.StatusWarn
		r.Message = fmt.Sprintf("No NICs matching rdma_type=%q found via ibstat", c.rdmaType) + otherNote
		return r
	}

	// All target NICs down → FAIL (hardware/cable issue).
	// Some target NICs down → WARN (partial degradation).
	// All target NICs active → PASS.
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
	return true
}

func (c *StatusCheck) rdmaTypeLabel() string {
	if c.rdmaType == "" {
		return "RDMA"
	}
	return string(c.rdmaType)
}

func (c *StatusCheck) otherRDMATypeLabel() string {
	switch c.rdmaType {
	case config.RDMATypeIB:
		return string(config.RDMATypeRoCE)
	case config.RDMATypeRoCE:
		return string(config.RDMATypeIB)
	default:
		return "non-matching"
	}
}

// NICStatusInfo holds parsed ibstat information for a single NIC port.
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
