package checks

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/config"
)

// ValidDeviceName matches safe RDMA device names (e.g., mlx5_0, ibp0).
// Must start with a letter or digit to reject argument-like strings ("-flag").
var ValidDeviceName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

// Check is the interface all validation checks must implement.
type Check interface {
	Name() string
	Category() string
	Run(ctx context.Context) Result
}

// Result represents the outcome of a single validation check.
type Result struct {
	Node        string `json:"node,omitempty"`
	Category    string `json:"category"`
	Name        string `json:"name"`
	Status      Status `json:"status"`
	Message     string `json:"message"`
	Remediation string `json:"remediation,omitempty"`
	Details     any    `json:"details,omitempty"`
}

// Status represents the result of a check.
type Status string

const (
	StatusPass Status = "PASS"
	StatusWarn Status = "WARN"
	StatusFail Status = "FAIL"
	StatusSkip Status = "SKIP"
)

// LinkLayer represents the RDMA link layer type reported by sysfs.
type LinkLayer string

const (
	LinkLayerInfiniBand LinkLayer = "InfiniBand"
	LinkLayerEthernet   LinkLayer = "Ethernet"
)

// PairingStrategy identifies the algorithm used to pair GPUs with NICs.
type PairingStrategy string

const (
	// PairingNUMAAffinity pairs by NUMA proximity (flat topology fallback).
	PairingNUMAAffinity PairingStrategy = "numa_affinity"
	// PairingPCIeDistance pairs 1:1 by shortest PCIe tree distance.
	PairingPCIeDistance PairingStrategy = "pcie_distance"
	// PairingNUMALoadBalance distributes GPUs across NICs within each NUMA.
	PairingNUMALoadBalance PairingStrategy = "numa_load_balance"
	// PairingBandwidthProbe pairs by measured intra-host loopback ib_write_bw bandwidth.
	PairingBandwidthProbe PairingStrategy = "intra-host_bandwidth"
)

// GPUInfo describes a single GPU with its PCIe location.
type GPUInfo struct {
	ID       int      `json:"id"`
	Name     string   `json:"name"`
	NUMA     int      `json:"numa"`
	PCIAddr  string   `json:"pci_addr"`
	PCIePath []string `json:"pcie_path,omitempty"`
}

// NICInfo describes a single RDMA NIC (HCA) with its PCIe location.
type NICInfo struct {
	Dev       string    `json:"dev"`
	NUMA      int       `json:"numa"`
	PCIAddr   string    `json:"pci_addr"`
	LinkLayer LinkLayer `json:"link_layer"`
	PCIePath  []string  `json:"pcie_path,omitempty"`
}

// GPUNICPair represents a GPU paired with its closest RDMA NIC.
// In memory it holds the full GPUInfo and NICInfo for controller use.
// JSON serialization outputs only the essential fields (gpu_id, gpu_numa,
// nic_dev, nic_numa, pcie_hops) to avoid repeating data already present
// in gpu_list/nic_list.
type GPUNICPair struct {
	GPU            GPUInfo
	NIC            NICInfo
	PCIeHops       int
	IntrahostBWGbps float64 // Measured intra-host loopback bandwidth; 0 means not measured.
}

// MarshalJSON outputs a slim representation of the pair.
func (p GPUNICPair) MarshalJSON() ([]byte, error) {
	slim := struct {
		GPUID           int     `json:"gpu_id"`
		GPUNUMA         int     `json:"gpu_numa"`
		NICDev          string  `json:"nic_dev"`
		NICNUMA         int     `json:"nic_numa"`
		PCIeHops        int     `json:"pcie_hops"`
		IntrahostBWGbps float64 `json:"intrahost_bandwidth_gbps"`
	}{
		GPUID:           p.GPU.ID,
		GPUNUMA:         p.GPU.NUMA,
		NICDev:          p.NIC.Dev,
		NICNUMA:         p.NIC.NUMA,
		PCIeHops:        p.PCIeHops,
		IntrahostBWGbps: p.IntrahostBWGbps,
	}
	return json.Marshal(slim)
}

// UnmarshalJSON reconstructs a GPUNICPair from slim JSON.
func (p *GPUNICPair) UnmarshalJSON(data []byte) error {
	var slim struct {
		GPUID           int      `json:"gpu_id"`
		GPUNUMA         int      `json:"gpu_numa"`
		NICDev          string   `json:"nic_dev"`
		NICNUMA         int      `json:"nic_numa"`
		PCIeHops        int      `json:"pcie_hops"`
		IntrahostBWGbps float64  `json:"intrahost_bandwidth_gbps"`
	}
	if err := json.Unmarshal(data, &slim); err != nil {
		return err
	}
	if slim.GPUID < 0 || slim.GPUNUMA < 0 || slim.NICNUMA < 0 || slim.PCIeHops < 0 {
		return fmt.Errorf("invalid GPUNICPair: negative value (gpu_id=%d, gpu_numa=%d, nic_numa=%d, pcie_hops=%d)",
			slim.GPUID, slim.GPUNUMA, slim.NICNUMA, slim.PCIeHops)
	}
	if !ValidDeviceName.MatchString(slim.NICDev) {
		return fmt.Errorf("invalid GPUNICPair: bad nic_dev %q", slim.NICDev)
	}
	p.GPU = GPUInfo{ID: slim.GPUID, NUMA: slim.GPUNUMA}
	p.NIC = NICInfo{Dev: slim.NICDev, NUMA: slim.NICNUMA}
	p.PCIeHops = slim.PCIeHops
	p.IntrahostBWGbps = slim.IntrahostBWGbps
	return nil
}

// NodeTopology holds the GPU-NIC-NUMA mapping for a node.
type NodeTopology struct {
	GPUNICPCIeMapping string          `json:"gpu_nic_pcie_mapping"`
	GPUCount          int             `json:"gpu_count"`
	NICCount          int             `json:"nic_count"`
	IsFlat            bool            `json:"is_flat"`
	PairingStrategy   PairingStrategy `json:"pairing_strategy"`
	GPUList           []GPUInfo       `json:"gpu_list,omitempty"`
	NICList           []NICInfo       `json:"nic_list,omitempty"`
	Pairs             []GPUNICPair    `json:"pairs"`
}

// NodeReport is the complete output from an agent run on a single node.
type NodeReport struct {
	Node      string    `json:"node"`
	Timestamp time.Time `json:"timestamp"`
	Results   []Result  `json:"results,omitempty"`
}

// NormalizeRDMAType validates and normalizes an RDMA type string.
// Returns the typed RDMAType if valid ("ib" or "roce"), empty for
// empty input, or an error for unrecognized values.
func NormalizeRDMAType(rdmaType string) (config.RDMAType, error) {
	rt := config.RDMAType(strings.ToLower(strings.TrimSpace(rdmaType)))
	if rt == "" {
		return "", nil
	}
	if rt == config.RDMATypeIB || rt == config.RDMATypeRoCE {
		return rt, nil
	}
	return "", fmt.Errorf("invalid RDMA_TYPE %q: must be %q, %q, or empty", rdmaType, config.RDMATypeIB, config.RDMATypeRoCE)
}

// ExtractTopology finds the gpu_nic_topology check result and deserializes
// its Details into a NodeTopology. Returns nil if not found.
func ExtractTopology(report NodeReport) *NodeTopology {
	for _, r := range report.Results {
		if r.Name != "gpu_nic_topology" || r.Details == nil {
			continue
		}
		// Details may be *NodeTopology (in-process) or map[string]any (from JSON)
		if topo, ok := r.Details.(*NodeTopology); ok {
			return topo
		}
		data, err := json.Marshal(r.Details)
		if err != nil {
			continue
		}
		var topo NodeTopology
		if json.Unmarshal(data, &topo) == nil {
			return &topo
		}
	}
	return nil
}
