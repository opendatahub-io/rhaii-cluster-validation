package rdma

import (
	"fmt"
	"io"
	"time"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/jobrunner"
)

// DevicesFromTopology extracts the list of unique NIC device names from topology Pairs.
func DevicesFromTopology(topo *checks.NodeTopology) []string {
	seen := make(map[string]bool)
	var devs []string
	for _, pair := range topo.Pairs {
		if !seen[pair.NIC.Dev] {
			devs = append(devs, pair.NIC.Dev)
			seen[pair.NIC.Dev] = true
		}
	}
	return devs
}

// BuildRailMap maps NIC device names to their rail index (position in topology Pairs).
func BuildRailMap(topo *checks.NodeTopology) map[string]int {
	m := make(map[string]int)
	if topo == nil {
		return m
	}
	for i, pair := range topo.Pairs {
		m[pair.NIC.Dev] = i
	}
	return m
}

// PingMeshStatus returns PASS/WARN/FAIL/SKIP based on passed/total counts.
func PingMeshStatus(passed, total int) checks.Status {
	switch {
	case total == 0:
		return checks.StatusSkip
	case passed == total:
		return checks.StatusPass
	case passed > 0:
		return checks.StatusWarn
	default:
		return checks.StatusFail
	}
}

// SkipPingMeshReport returns a PingMeshReport with SKIP status for both checks.
func SkipPingMeshReport(message string) *PingMeshReport {
	return &PingMeshReport{
		Summary: map[string]PingMeshCheckSummary{
			"rdma_conn_rail":  {Status: checks.StatusSkip, Message: message},
			"rdma_conn_xrail": {Status: checks.StatusSkip, Message: message},
		},
	}
}

// ClassifyPingMeshResults processes jobrunner.RunPairwise results into a
// PingMeshReport and PingMeshFailuresReport, classifying each NIC pair as
// rail (same position in both nodes' topology) or cross-rail. Warnings about
// missing topology data are written to output.
func ClassifyPingMeshResults(
	pairResults map[jobrunner.NodePair][]jobrunner.JobResult,
	topoMap map[string]*checks.NodeTopology,
	output io.Writer,
) (*PingMeshReport, *PingMeshFailuresReport) {
	var (
		railPassed, railTotal   int
		xrailPassed, xrailTotal int
		matrix                  []PingMeshNodePair
		allFailures             []PingMeshFailure
		nodePairCount           int
	)

	for pair, attempts := range pairResults {
		nodePairCount++
		serverTopo, okS := topoMap[pair.Server]
		clientTopo, okC := topoMap[pair.Client]
		if !okS || !okC {
			fmt.Fprintf(output, "  Warning: missing topology for %s or %s in classification, skipping pair\n", pair.Server, pair.Client)
			continue
		}

		serverRails := BuildRailMap(serverTopo)
		clientRails := BuildRailMap(clientTopo)

		// Merge results across retry attempts: a NIC pair passes if it succeeded in any attempt
		type nicPairKey struct{ src, dst string }
		bestResult := make(map[nicPairKey]bool)
		lastError := make(map[nicPairKey]string)
		lastAttempt := make(map[nicPairKey]int)

		for attemptIdx, jr := range attempts {
			results, ok := jr.Details.([]PingMeshPairResult)
			if !ok {
				continue
			}
			for _, r := range results {
				k := nicPairKey{src: r.SrcDev, dst: r.DstDev}
				if r.Pass {
					bestResult[k] = true
				}
				if !r.Pass {
					lastError[k] = r.Error
					lastAttempt[k] = attemptIdx + 1
				}
				if _, exists := bestResult[k]; !exists {
					bestResult[k] = false
				}
			}
		}

		var npRail, npXRail, npAll PingMeshCount

		for k, passed := range bestResult {
			srcRail, srcOk := clientRails[k.src]
			dstRail, dstOk := serverRails[k.dst]

			isRail := srcOk && dstOk && srcRail == dstRail
			cat := PingMeshCategoryXRail
			if isRail {
				cat = PingMeshCategoryRail
			}

			npAll.Total++
			if isRail {
				npRail.Total++
				railTotal++
			} else {
				npXRail.Total++
				xrailTotal++
			}

			if passed {
				npAll.Passed++
				if isRail {
					npRail.Passed++
					railPassed++
				} else {
					npXRail.Passed++
					xrailPassed++
				}
			} else {
				allFailures = append(allFailures, PingMeshFailure{
					NodeA:    pair.Server,
					NodeB:    pair.Client,
					SrcDev:   k.src,
					DstDev:   k.dst,
					Category: cat,
					Error:    lastError[k],
					Attempt:  lastAttempt[k],
				})
			}
		}

		nodeA, nodeB := pair.Server, pair.Client
		if nodeA > nodeB {
			nodeA, nodeB = nodeB, nodeA
		}
		matrix = append(matrix, PingMeshNodePair{
			NodeA: nodeA,
			NodeB: nodeB,
			Rail:  npRail,
			XRail: npXRail,
			All:   npAll,
		})
	}

	report := &PingMeshReport{
		Summary: map[string]PingMeshCheckSummary{
			"rdma_conn_rail": {
				Status:  PingMeshStatus(railPassed, railTotal),
				Passed:  railPassed,
				Total:   railTotal,
				Message: fmt.Sprintf("Rail RDMA connectivity: %d/%d NIC pairs across %d node pairs", railPassed, railTotal, nodePairCount),
			},
			"rdma_conn_xrail": {
				Status:  PingMeshStatus(xrailPassed, xrailTotal),
				Passed:  xrailPassed,
				Total:   xrailTotal,
				Message: fmt.Sprintf("Cross-rail RDMA connectivity: %d/%d NIC pairs across %d node pairs", xrailPassed, xrailTotal, nodePairCount),
			},
		},
		Matrix: matrix,
	}

	return report, &PingMeshFailuresReport{
		Timestamp: time.Now().UTC(),
		Failures:  allFailures,
	}
}
