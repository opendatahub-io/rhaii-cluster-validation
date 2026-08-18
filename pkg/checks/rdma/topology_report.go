package rdma

import (
	"fmt"
	"io"
	"strings"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
)

// BuildTopologyMap extracts topology from node reports, keyed by node name.
func BuildTopologyMap(reports []checks.NodeReport) map[string]*checks.NodeTopology {
	m := make(map[string]*checks.NodeTopology)
	for _, r := range reports {
		if topo := checks.ExtractTopology(r); topo != nil {
			m[r.Node] = topo
		}
	}
	return m
}

// TopologyCoversAllNodes returns true if every node in gpuNodes has topology data in reports.
func TopologyCoversAllNodes(reports []checks.NodeReport, gpuNodes []string) bool {
	topoMap := BuildTopologyMap(reports)
	for _, n := range gpuNodes {
		if _, ok := topoMap[n]; !ok {
			return false
		}
	}
	return true
}

// NeedsBandwidthProbe returns true if any node used NUMA-affinity pairing,
// meaning PCIe-based pairing was not possible (flat topology or missing PCIe paths).
func NeedsBandwidthProbe(reports []checks.NodeReport) bool {
	for _, r := range reports {
		if topo := checks.ExtractTopology(r); topo != nil && topo.PairingStrategy == checks.PairingNUMAAffinity {
			return true
		}
	}
	return false
}

// WarnUnpairedFlatNodes marks the topology check as WARN on flat-topology nodes
// that still use NUMA-affinity pairing after the BW probe phase. This indicates
// the BW probe either failed or was skipped for that node.
func WarnUnpairedFlatNodes(reports []checks.NodeReport) []checks.NodeReport {
	for i, r := range reports {
		topo := checks.ExtractTopology(r)
		if topo == nil || !topo.IsFlat || topo.PairingStrategy != checks.PairingNUMAAffinity {
			continue
		}
		for j, res := range reports[i].Results {
			if res.Name == "gpu_nic_topology" && res.Status == checks.StatusPass {
				reports[i].Results[j].Status = checks.StatusWarn
				reports[i].Results[j].Message += " (BW probe unavailable; using NUMA-affinity fallback)"
				break
			}
		}
	}
	return reports
}

// ApplyBandwidthPairing replaces NUMA-affinity pairs with bandwidth-optimal
// pairs for nodes that have loopback BW probe results. Progress and warning
// messages are written to output.
func ApplyBandwidthPairing(netReports []checks.NodeReport, bwResults map[string]*LoopbackBWReport, output io.Writer) []checks.NodeReport {
	for i, report := range netReports {
		bwReport, ok := bwResults[report.Node]
		if !ok {
			continue
		}

		topo := checks.ExtractTopology(report)
		if topo == nil || topo.PairingStrategy != checks.PairingNUMAAffinity {
			continue
		}

		newPairs := BandwidthOptimalPairing(bwReport.Results, topo.GPUList, topo.NICList)
		if len(newPairs) == 0 {
			fmt.Fprintf(output, "  Warning: BW probe pairing produced no pairs for %s, keeping NUMA-affinity pairing\n", report.Node)
			continue
		}

		topo.Pairs = newPairs
		topo.PairingStrategy = checks.PairingBandwidthProbe
		topo.GPUNICPCIeMapping = BuildGPUNICPCIeMapping(newPairs)

		// Update the topology result: Details and Message
		var pairDescs []string
		for _, p := range newPairs {
			pairDescs = append(pairDescs, fmt.Sprintf("GPU%d↔%s(NUMA:%d↔%d)", p.GPU.ID, p.NIC.Dev, p.GPU.NUMA, p.NIC.NUMA))
		}
		updatedMsg := fmt.Sprintf("%d GPU(s), %d NIC(s), strategy=%s: %s",
			len(topo.GPUList), len(topo.NICList), topo.PairingStrategy, strings.Join(pairDescs, ", "))
		for j, res := range netReports[i].Results {
			if res.Name == "gpu_nic_topology" {
				netReports[i].Results[j].Details = topo
				netReports[i].Results[j].Message = updatedMsg
				break
			}
		}

		fmt.Fprintf(output, "  Updated %s pairing via bandwidth probe:\n", report.Node)
		for _, p := range newPairs {
			fmt.Fprintf(output, "    GPU%d ↔ %s (%.1f Gbps)\n", p.GPU.ID, p.NIC.Dev, p.IntrahostBWGbps)
		}
	}
	return netReports
}
