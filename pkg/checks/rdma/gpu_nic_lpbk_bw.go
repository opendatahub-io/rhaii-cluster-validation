package rdma

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
)

const (
	DefaultLoopbackIters      = 5000
	DefaultLoopbackMsgSize    = 1048576 // 1 MiB
	DefaultLoopbackQPs        = 4       // Queue pairs needed to saturate 400 Gbps NICs
	DefaultPerTestTimeoutSecs = 30
	// Base port for ib_write_bw loopback server. Each GPU-NIC pair increments
	// from this base (e.g., 8×8 matrix uses ports 20000–20063). Port collisions
	// across nodes are not possible because loopback tests bind to localhost only.
	DefaultLoopbackBasePort = 20000
)

// LoopbackBWEntry holds the measured loopback bandwidth for one GPU-NIC pair.
type LoopbackBWEntry struct {
	GPUId  int     `json:"gpu_id"`
	NICDev string  `json:"nic_dev"`
	BWGbps float64 `json:"bw_gbps"`
	Error  string  `json:"error,omitempty"`
}

// LoopbackBWReport holds the loopback bandwidth matrix for one node.
type LoopbackBWReport struct {
	Node    string
	Results []LoopbackBWEntry
}

// loopbackJSON mirrors the JSON structure emitted by the bash script.
type loopbackJSON struct {
	Results []LoopbackBWEntry `json:"results"`
}

// BuildLoopbackScript generates a bash script that runs ib_write_bw loopback
// tests for every GPU-NIC combination and emits a JSON bandwidth matrix.
// qps sets the number of queue pairs (-q flag); use 0 for the default (4).
// Returns an error if any NIC device name fails validation.
func BuildLoopbackScript(gpuIDs []int, nicDevs []string, iters, msgSize, perTestTimeout, qps int) (string, error) {
	if len(gpuIDs) == 0 {
		return "", fmt.Errorf("no GPU IDs provided")
	}
	if len(nicDevs) == 0 {
		return "", fmt.Errorf("no NIC devices provided")
	}
	for _, dev := range nicDevs {
		if !checks.ValidDeviceName.MatchString(dev) {
			return "", fmt.Errorf("invalid NIC device name %q: must match %s", dev, checks.ValidDeviceName.String())
		}
	}
	if iters <= 0 {
		iters = DefaultLoopbackIters
	}
	if msgSize <= 0 {
		msgSize = DefaultLoopbackMsgSize
	}
	if perTestTimeout <= 0 {
		perTestTimeout = DefaultPerTestTimeoutSecs
	}
	if qps <= 0 {
		qps = DefaultLoopbackQPs
	}

	gpuStrs := make([]string, len(gpuIDs))
	for i, id := range gpuIDs {
		gpuStrs[i] = fmt.Sprintf("%d", id)
	}

	var sb strings.Builder
	sb.WriteString("#!/bin/bash\n")
	sb.WriteString(fmt.Sprintf("GPU_IDS='%s'\n", strings.Join(gpuStrs, ",")))
	sb.WriteString(fmt.Sprintf("NIC_DEVS='%s'\n", strings.Join(nicDevs, ",")))
	sb.WriteString(fmt.Sprintf("iters=%d\n", iters))
	sb.WriteString(fmt.Sprintf("msg_size=%d\n", msgSize))
	sb.WriteString(fmt.Sprintf("num_qps=%d\n", qps))
	sb.WriteString(fmt.Sprintf("per_test_timeout=%d\n", perTestTimeout))
	sb.WriteString(fmt.Sprintf("base_port=%d\n", DefaultLoopbackBasePort))
	sb.WriteString(`
IFS=',' read -ra GPUS <<< "$GPU_IDS"
IFS=',' read -ra NICS <<< "$NIC_DEVS"

echo '{"results":['
first=true
for gpu in "${GPUS[@]}"; do
  for nic in "${NICS[@]}"; do
    port=$((base_port++))
    bw=0
    error_msg=""

    timeout "$per_test_timeout" ib_write_bw -d "$nic" --use_cuda "$gpu" \
      -p "$port" -n "$iters" --size "$msg_size" -q "$num_qps" --report_gbits > /dev/null 2>&1 &
    srv=$!
    sleep 2

    json_file="/tmp/bw_${gpu}_${nic}.json"
    timeout "$per_test_timeout" ib_write_bw -d "$nic" --use_cuda "$gpu" \
      -p "$port" -n "$iters" --size "$msg_size" -q "$num_qps" --report_gbits \
      --out_json --out_json_file="$json_file" localhost > /dev/null 2>&1
    rc=$?

    if [ $rc -eq 124 ]; then
      error_msg="timeout after ${per_test_timeout}s"
    elif [ $rc -ne 0 ]; then
      error_msg="exit code $rc"
    elif [ -f "$json_file" ]; then
      bw=$(grep -o '"BW_average"[[:space:]]*:[[:space:]]*[0-9.]*' "$json_file" | grep -o '[0-9.]*$')
      [ -z "$bw" ] && bw=0
    fi
    rm -f "$json_file"

    kill "$srv" 2>/dev/null
    wait "$srv" 2>/dev/null

    [ "$first" = true ] && first=false || printf ','
    if [ -n "$error_msg" ]; then
      printf '{"gpu_id":%d,"nic_dev":"%s","bw_gbps":0,"error":"%s"}\n' "$gpu" "$nic" "$error_msg"
    else
      printf '{"gpu_id":%d,"nic_dev":"%s","bw_gbps":%s}\n' "$gpu" "$nic" "$bw"
    fi
  done
done
echo ']}'
`)
	return sb.String(), nil
}

// ParseLoopbackBWOutput extracts the JSON bandwidth report from pod logs.
// The bash script outputs {"results":[...]} but ib_write_bw also writes to
// stdout, so we search for the last occurrence of {"results": to find our JSON.
func ParseLoopbackBWOutput(logs string) ([]LoopbackBWEntry, error) {
	marker := `{"results":[`
	idx := strings.LastIndex(logs, marker)
	if idx < 0 {
		return nil, fmt.Errorf("no JSON report found in loopback BW output")
	}

	var report loopbackJSON
	if err := json.Unmarshal([]byte(logs[idx:]), &report); err != nil {
		return nil, fmt.Errorf("failed to parse loopback BW JSON: %w", err)
	}
	if len(report.Results) == 0 {
		return nil, fmt.Errorf("loopback BW report has no results")
	}
	return report.Results, nil
}

// BandwidthOptimalPairing finds a high-quality 1:1 GPU-NIC assignment that
// maximizes total bandwidth using a greedy algorithm. Entries are sorted by
// descending bandwidth, and each GPU-NIC pair is assigned greedily if neither
// the GPU nor the NIC is already taken. This produces optimal results for the
// common diagonal-dominant bandwidth matrix (GPU_i has highest BW to NIC_i)
// and near-optimal results for non-diagonal cases.
//
// Phase 2 handles leftover GPUs (when GPUs > NICs or when sparse BW data
// leaves some GPUs unmatched in Phase 1). Only NICs already assigned in
// Phase 1 are candidates — Phase 2 reuses them. For each leftover GPU,
// the NIC (among those Phase 1 winners) that showed the highest measured
// bandwidth to this specific GPU is selected. If no BW data exists for a
// GPU, it round-robins across the Phase 1 NICs.
func BandwidthOptimalPairing(bwEntries []LoopbackBWEntry, gpus []checks.GPUInfo, nics []checks.NICInfo) []checks.GPUNICPair {
	if len(gpus) == 0 || len(nics) == 0 {
		return nil
	}

	gpuMap := make(map[int]checks.GPUInfo, len(gpus))
	for _, g := range gpus {
		gpuMap[g.ID] = g
	}
	nicMap := make(map[string]checks.NICInfo, len(nics))
	for _, n := range nics {
		nicMap[n.Dev] = n
	}

	sorted := make([]LoopbackBWEntry, len(bwEntries))
	copy(sorted, bwEntries)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].BWGbps != sorted[j].BWGbps {
			return sorted[i].BWGbps > sorted[j].BWGbps
		}
		if sorted[i].GPUId != sorted[j].GPUId {
			return sorted[i].GPUId < sorted[j].GPUId
		}
		return sorted[i].NICDev < sorted[j].NICDev
	})

	assignedGPU := make(map[int]bool)
	assignedNIC := make(map[string]bool)
	var pairs []checks.GPUNICPair
	limit := len(gpus)
	if len(nics) < limit {
		limit = len(nics)
	}

	type gpuNICKey struct {
		gpuID  int
		nicDev string
	}
	bwLookup := make(map[gpuNICKey]float64, len(bwEntries))
	for _, e := range bwEntries {
		bwLookup[gpuNICKey{e.GPUId, e.NICDev}] = e.BWGbps
	}

	// Phase 1: greedy 1:1 assignment
	for _, e := range sorted {
		if len(pairs) >= limit {
			break
		}
		if assignedGPU[e.GPUId] || assignedNIC[e.NICDev] {
			continue
		}
		gpu, gOK := gpuMap[e.GPUId]
		nic, nOK := nicMap[e.NICDev]
		if !gOK || !nOK {
			continue
		}
		pairs = append(pairs, checks.GPUNICPair{GPU: gpu, NIC: nic, PCIeHops: 0, IntrahostBWGbps: e.BWGbps})
		assignedGPU[e.GPUId] = true
		assignedNIC[e.NICDev] = true
	}

	// Phase 2: assign leftover GPUs to the highest-bandwidth NIC from Phase 1.
	// This handles GPUs > NICs and sparse BW matrices where Phase 1 couldn't
	// match every GPU (e.g., 5 of 8 GPUs matched due to missing entries).
	if len(pairs) < len(gpus) && len(pairs) > 0 {
		pairedNICSet := make(map[string]checks.NICInfo, len(pairs))
		for _, p := range pairs {
			pairedNICSet[p.NIC.Dev] = p.NIC
		}

		// Build per-GPU lookup: GPU -> best BW among paired NICs
		gpuBestNIC := make(map[int]LoopbackBWEntry)
		for _, e := range sorted {
			if _, ok := pairedNICSet[e.NICDev]; !ok {
				continue
			}
			if prev, exists := gpuBestNIC[e.GPUId]; !exists || e.BWGbps > prev.BWGbps {
				gpuBestNIC[e.GPUId] = e
			}
		}

		pairedNICs := make([]checks.NICInfo, 0, len(pairs))
		for _, p := range pairs {
			pairedNICs = append(pairedNICs, p.NIC)
		}
		leftover := make([]checks.GPUInfo, 0, len(gpus)-len(pairs))
		for _, g := range gpus {
			if !assignedGPU[g.ID] {
				leftover = append(leftover, g)
			}
		}
		sort.Slice(leftover, func(i, j int) bool { return leftover[i].ID < leftover[j].ID })
		rrIdx := 0
		for _, g := range leftover {
			if best, ok := gpuBestNIC[g.ID]; ok && best.BWGbps > 0 {
				pairs = append(pairs, checks.GPUNICPair{
					GPU:             g,
					NIC:             pairedNICSet[best.NICDev],
					PCIeHops:        0,
					IntrahostBWGbps: best.BWGbps,
				})
			} else {
				bw := bwLookup[gpuNICKey{g.ID, pairedNICs[rrIdx%len(pairedNICs)].Dev}]
				pairs = append(pairs, checks.GPUNICPair{
					GPU:             g,
					NIC:             pairedNICs[rrIdx%len(pairedNICs)],
					PCIeHops:        0,
					IntrahostBWGbps: bw,
				})
				rrIdx++
			}
		}
	}

	sort.Slice(pairs, func(i, j int) bool { return pairs[i].GPU.ID < pairs[j].GPU.ID })
	return pairs
}

// BuildGPUNICPCIeMapping regenerates the PCIe mapping string from pairs,
// using the same format as topology.go: "gpuPCI=nicPCI,gpuPCI=nicPCI,...".
func BuildGPUNICPCIeMapping(pairs []checks.GPUNICPair) string {
	var mappings []string
	for _, p := range pairs {
		mappings = append(mappings, fmt.Sprintf("%s=%s", p.GPU.PCIAddr, p.NIC.PCIAddr))
	}
	return strings.Join(mappings, ",")
}
