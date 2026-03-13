package rdma

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
)

// StatusCheck validates RDMA NIC link state and speed.
type StatusCheck struct {
	nodeName string
}

func NewStatusCheck(nodeName string) *StatusCheck {
	return &StatusCheck{nodeName: nodeName}
}

func (c *StatusCheck) Name() string     { return "rdma_nic_status" }
func (c *StatusCheck) Category() string { return "networking_rdma" }

func (c *StatusCheck) Run(ctx context.Context) checks.Result {
	r := checks.Result{
		Node:     c.nodeName,
		Category: c.Category(),
		Name:     c.Name(),
	}

	output, err := exec.CommandContext(ctx, "ibstat").Output()
	if err != nil {
		r.Status = checks.StatusFail
		r.Message = fmt.Sprintf("ibstat failed: %v", err)
		r.Remediation = "Install rdma-core / infiniband-diags"
		return r
	}

	nics := parseIBStat(string(output))
	if len(nics) == 0 {
		r.Status = checks.StatusFail
		r.Message = "No RDMA NICs found via ibstat"
		return r
	}

	var downNICs []string
	var activeNICs []string
	for _, nic := range nics {
		if nic.State == "Active" {
			activeNICs = append(activeNICs, fmt.Sprintf("%s (%s Gbps)", nic.Name, nic.Rate))
		} else {
			downNICs = append(downNICs, nic.Name)
		}
	}

	r.Details = map[string]any{"nics": nics}

	if len(downNICs) > 0 {
		r.Status = checks.StatusFail
		r.Message = fmt.Sprintf("RDMA NIC(s) down: %s", strings.Join(downNICs, ", "))
		r.Remediation = "Check InfiniBand cable and switch configuration"
		return r
	}

	r.Status = checks.StatusPass
	r.Message = fmt.Sprintf("%d active RDMA NIC(s): %s", len(activeNICs), strings.Join(activeNICs, ", "))
	return r
}

// NICInfo holds parsed ibstat information for a single NIC.
type NICInfo struct {
	Name  string `json:"name"`
	State string `json:"state"`
	Rate  string `json:"rate"`
}

func parseIBStat(output string) []NICInfo {
	var nics []NICInfo

	nameRe := regexp.MustCompile(`CA '([^']+)'`)
	portRe := regexp.MustCompile(`Port (\d+):`)
	stateRe := regexp.MustCompile(`State:\s+(\S+)`)
	rateRe := regexp.MustCompile(`Rate:\s+(\S+)`)

	sections := strings.Split(output, "CA '")
	for _, section := range sections[1:] {
		caName := ""
		if m := nameRe.FindStringSubmatch("CA '" + section); len(m) > 1 {
			caName = m[1]
		}
		if caName == "" {
			continue
		}

		// Split by port sections within this CA
		portSections := portRe.Split(section, -1)
		portNumbers := portRe.FindAllStringSubmatch(section, -1)

		if len(portSections) <= 1 {
			// No port info, treat whole section as single port
			nic := NICInfo{Name: caName}
			if m := stateRe.FindStringSubmatch(section); len(m) > 1 {
				nic.State = m[1]
			}
			if m := rateRe.FindStringSubmatch(section); len(m) > 1 {
				nic.Rate = m[1]
			}
			nics = append(nics, nic)
			continue
		}

		// Parse each port
		for i, portSection := range portSections[1:] {
			portNum := ""
			if i < len(portNumbers) {
				portNum = portNumbers[i][1]
			}

			nic := NICInfo{Name: fmt.Sprintf("%s/port%s", caName, portNum)}
			if m := stateRe.FindStringSubmatch(portSection); len(m) > 1 {
				nic.State = m[1]
			}
			if m := rateRe.FindStringSubmatch(portSection); len(m) > 1 {
				nic.Rate = m[1]
			}
			nics = append(nics, nic)
		}
	}

	return nics
}
