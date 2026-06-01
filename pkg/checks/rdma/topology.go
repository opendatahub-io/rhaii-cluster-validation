package rdma

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/config"
)

// crossNUMAPenalty is added to PCIeHops when a GPU-NIC pair crosses NUMA
// boundaries, making cross-NUMA assignments immediately visible in output.
const crossNUMAPenalty = 100

// unknownPathPenalty is the PCIe distance assigned when one or both devices
// lack sysfs path data. Set between typical within-NUMA distances (~2-12)
// and the cross-NUMA penalty (100), so unknown-path devices sort after real
// matches but before cross-NUMA fallbacks.
const unknownPathPenalty = 50

// TopologyCheck discovers GPU-NIC-NUMA-PCIe mapping on the node.
type TopologyCheck struct {
	nodeName string
	rdmaType config.RDMAType
}

func NewTopologyCheck(nodeName string, rdmaType config.RDMAType) *TopologyCheck {
	return &TopologyCheck{nodeName: nodeName, rdmaType: rdmaType}
}

func (c *TopologyCheck) Name() string     { return "gpu_nic_topology" }
func (c *TopologyCheck) Category() string { return "networking_rdma" }

func (c *TopologyCheck) Run(ctx context.Context) checks.Result {
	r := checks.Result{
		Node:     c.nodeName,
		Category: c.Category(),
		Name:     c.Name(),
	}

	vendor := os.Getenv("GPU_VENDOR")

	gpus, err := discoverGPUs(ctx, vendor)
	if err != nil {
		r.Status = checks.StatusSkip
		r.Message = fmt.Sprintf("GPU discovery failed: %v", err)
		return r
	}

	nics, err := discoverNICs(ctx, c.rdmaType)
	if err != nil {
		r.Status = checks.StatusSkip
		r.Message = fmt.Sprintf("NIC discovery failed: %v", err)
		return r
	}

	pairs, flat, strategy := buildPairs(gpus, nics)

	var mappings []string
	for _, p := range pairs {
		mappings = append(mappings, fmt.Sprintf("%s=%s", p.GPU.PCIAddr, p.NIC.PCIAddr))
	}

	topo := &checks.NodeTopology{
		GPUNICPCIeMapping: strings.Join(mappings, ","),
		GPUCount:          len(gpus),
		NICCount:          len(nics),
		IsFlat:            flat,
		PairingStrategy:   strategy,
		GPUList:           gpus,
		NICList:           nics,
		Pairs:             pairs,
	}

	r.Details = topo

	switch {
	case len(nics) == 0:
		r.Status = checks.StatusWarn
		if c.rdmaType == "" {
			r.Message = fmt.Sprintf("%d GPU(s), 0 NIC(s): no RDMA-capable NICs found", len(gpus))
		} else {
			r.Message = fmt.Sprintf("%d GPU(s), 0 NIC(s): no RDMA NICs found matching rdma_type=%q", len(gpus), c.rdmaType)
		}
		return r
	case len(gpus) > len(nics):
		r.Status = checks.StatusWarn
	default:
		r.Status = checks.StatusPass
	}

	var pairDescs []string
	for _, p := range pairs {
		pairDescs = append(pairDescs, fmt.Sprintf("GPU%d↔%s(NUMA:%d↔%d)", p.GPU.ID, p.NIC.Dev, p.GPU.NUMA, p.NIC.NUMA))
	}
	r.Message = fmt.Sprintf("%d GPU(s), %d NIC(s), strategy=%s: %s",
		len(gpus), len(nics), strategy, strings.Join(pairDescs, ", "))

	return r
}

// sysfsExec runs a command directly (no chroot). Privileged containers
// see the host's /sys natively, so chroot is unnecessary for sysfs reads.
func sysfsExec(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

// pciAddrRegex matches a well-formed PCI BDF address (DDDD:BB:DD.F, hex).
var pciAddrRegex = regexp.MustCompile(`^[0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-9a-f]$`)

func validPCIAddr(addr string) bool {
	return pciAddrRegex.MatchString(addr)
}

// ---------------------------------------------------------------------------
// GPU discovery
// ---------------------------------------------------------------------------

func discoverGPUs(ctx context.Context, vendor string) ([]checks.GPUInfo, error) {
	switch vendor {
	case "amd":
		return discoverAMDGPUs(ctx)
	default:
		return discoverNVIDIAGPUs(ctx)
	}
}

func discoverNVIDIAGPUs(ctx context.Context) ([]checks.GPUInfo, error) {
	output, err := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=index,gpu_name,pci.bus_id",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi failed: %w", err)
	}

	var gpus []checks.GPUInfo
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		fields := strings.SplitN(line, ",", 3)
		if len(fields) < 3 {
			continue
		}
		id, err := strconv.Atoi(strings.TrimSpace(fields[0]))
		if err != nil || id < 0 || id > 255 {
			continue
		}
		name := strings.TrimSpace(fields[1])
		pciAddr := strings.TrimSpace(fields[2])

		// nvidia-smi returns 8-digit domain (00000000:19:00.0), sysfs uses 4-digit (0000:19:00.0)
		sysfsAddr := normalizePCIAddr(pciAddr)

		numa := -1
		var pciePath []string
		if validPCIAddr(sysfsAddr) {
			numaOutput, err := sysfsExec(ctx, "cat",
				fmt.Sprintf("/sys/bus/pci/devices/%s/numa_node", sysfsAddr))
			if err == nil {
				numa, _ = strconv.Atoi(strings.TrimSpace(string(numaOutput)))
			}
			pciePath = discoverPCIePath(ctx, sysfsAddr)
		} else {
			sysfsAddr = ""
		}

		gpus = append(gpus, checks.GPUInfo{ID: id, Name: name, NUMA: numa, PCIAddr: sysfsAddr, PCIePath: pciePath})
	}

	return gpus, nil
}

// normalizePCIAddr converts an nvidia-smi PCI address (00000000:3B:00.0)
// to the lowercase 4-digit domain format used by sysfs (0000:3b:00.0).
func normalizePCIAddr(addr string) string {
	addr = strings.ToLower(addr)
	parts := strings.SplitN(addr, ":", 2)
	if len(parts) == 2 && len(parts[0]) > 4 {
		return parts[0][len(parts[0])-4:] + ":" + parts[1]
	}
	return addr
}

// discoverAMDGPUs uses amd-smi to list GPUs with PCIe addresses.
// Expected output format from "amd-smi list":
//
//	GPU: 0
//	    BDF: 0000:0c:00.0
//	    UUID: ...
func discoverAMDGPUs(ctx context.Context) ([]checks.GPUInfo, error) {
	output, err := exec.CommandContext(ctx, "amd-smi", "list").Output()
	if err != nil {
		return nil, fmt.Errorf("amd-smi list failed: %w", err)
	}

	var gpus []checks.GPUInfo
	var current *checks.GPUInfo

	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "GPU:") {
			if current != nil {
				gpus = append(gpus, *current)
			}
			idStr := strings.TrimSpace(strings.TrimPrefix(line, "GPU:"))
			id, err := strconv.Atoi(idStr)
			if err != nil {
				current = nil
				continue
			}
			current = &checks.GPUInfo{ID: id, Name: "AMD GPU"}
		} else if strings.HasPrefix(line, "BDF:") && current != nil {
			current.PCIAddr = strings.TrimSpace(strings.TrimPrefix(line, "BDF:"))
		}
	}
	if current != nil {
		gpus = append(gpus, *current)
	}

	for i, g := range gpus {
		if g.PCIAddr == "" || !validPCIAddr(g.PCIAddr) {
			gpus[i].PCIAddr = ""
			gpus[i].NUMA = -1
			continue
		}
		numaOutput, err := sysfsExec(ctx, "cat",
			fmt.Sprintf("/sys/bus/pci/devices/%s/numa_node", g.PCIAddr))
		if err == nil {
			gpus[i].NUMA, _ = strconv.Atoi(strings.TrimSpace(string(numaOutput)))
		} else {
			gpus[i].NUMA = -1
		}
		gpus[i].PCIePath = discoverPCIePath(ctx, g.PCIAddr)
	}

	return gpus, nil
}

// ---------------------------------------------------------------------------
// NIC discovery
// ---------------------------------------------------------------------------

// discoverNICs finds RDMA devices with PCIe addresses and link layer type.
// rdmaType filters by link type: "ib" keeps InfiniBand, "roce" keeps Ethernet,
// empty keeps all.
func discoverNICs(ctx context.Context, rdmaType config.RDMAType) ([]checks.NICInfo, error) {
	output, err := sysfsExec(ctx, "ls", "/sys/class/infiniband/")
	if err != nil {
		return nil, fmt.Errorf("no infiniband devices: %w", err)
	}

	var nics []checks.NICInfo
	for _, dev := range strings.Fields(strings.TrimSpace(string(output))) {
		dev = strings.TrimSpace(dev)
		if dev == "" {
			continue
		}

		numaPath := filepath.Join("/sys/class/infiniband", dev, "device/numa_node")
		numaOutput, err := sysfsExec(ctx, "cat", numaPath)
		numa := -1
		if err == nil {
			numa, _ = strconv.Atoi(strings.TrimSpace(string(numaOutput)))
		}

		// readlink -f gives the full sysfs path: PCI address is the last segment,
		// and the full set of segments is the PCIe hierarchy.
		pciAddr := ""
		var pciePath []string
		fullPathOutput, err := sysfsExec(ctx, "readlink", "-f",
			filepath.Join("/sys/class/infiniband", dev, "device"))
		if err == nil {
			fullPath := strings.TrimSpace(string(fullPathOutput))
			pciAddr = filepath.Base(fullPath)
			pciePath = parsePCIePath(fullPath)
		}

		var linkLayer checks.LinkLayer
		llOutput, err := sysfsExec(ctx, "cat",
			filepath.Join("/sys/class/infiniband", dev, "ports/1/link_layer"))
		if err == nil {
			linkLayer = checks.LinkLayer(strings.TrimSpace(string(llOutput)))
		}

		if pciAddr != "" && !validPCIAddr(pciAddr) {
			pciAddr = ""
			pciePath = nil
		}
		if !hasRDMACapability(ctx, dev) {
			continue
		}
		if rdmaType == config.RDMATypeIB && linkLayer != checks.LinkLayerInfiniBand {
			continue
		}
		if rdmaType == config.RDMATypeRoCE && linkLayer != checks.LinkLayerEthernet {
			continue
		}

		nics = append(nics, checks.NICInfo{Dev: dev, NUMA: numa, PCIAddr: pciAddr, LinkLayer: linkLayer, PCIePath: pciePath})
	}

	sort.Slice(nics, func(i, j int) bool { return nics[i].Dev < nics[j].Dev })

	return nics, nil
}

// ---------------------------------------------------------------------------
// PCIe hierarchy helpers
// ---------------------------------------------------------------------------

// discoverPCIePath resolves the full sysfs path for a PCI device and returns
// the hierarchy as path segments.
func discoverPCIePath(ctx context.Context, pciAddr string) []string {
	output, err := sysfsExec(ctx, "readlink", "-f",
		fmt.Sprintf("/sys/bus/pci/devices/%s", pciAddr))
	if err != nil {
		return nil
	}
	return parsePCIePath(strings.TrimSpace(string(output)))
}

// parsePCIePath extracts PCI address segments from a sysfs device path.
// Input:  /sys/devices/pci0000:37/0000:37:00.0/0000:38:00.0/0000:3b:00.0
// Output: ["0000:37:00.0", "0000:38:00.0", "0000:3b:00.0"]
func parsePCIePath(sysfsPath string) []string {
	parts := strings.Split(sysfsPath, "/")
	var segments []string
	foundRoot := false
	for _, p := range parts {
		if strings.HasPrefix(p, "pci") {
			foundRoot = true
			continue
		}
		if foundRoot && strings.Contains(p, ":") {
			segments = append(segments, p)
		}
	}
	return segments
}

// pcieSharedPrefix returns the length of the longest shared prefix between
// two PCIe paths.
func pcieSharedPrefix(a, b []string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// pcieDistance computes the number of hops between two devices in the PCIe tree.
// distance = hops up to common ancestor + hops down.
func pcieDistance(a, b []string) int {
	shared := pcieSharedPrefix(a, b)
	return (len(a) - shared) + (len(b) - shared)
}

// isFlat returns true when all device PCIe paths have at most 1 segment,
// indicating a flat/virtual PCI bus with no hierarchy.
func isFlat(gpus []checks.GPUInfo, nics []checks.NICInfo) bool {
	for _, g := range gpus {
		if len(g.PCIePath) > 1 {
			return false
		}
	}
	for _, n := range nics {
		if len(n.PCIePath) > 1 {
			return false
		}
	}
	return true
}

// hasPCIePaths returns true if at least some devices have multi-segment
// PCIe path information suitable for distance calculations. Devices without
// paths get unknownPathPenalty in buildCandidates, preventing them from
// appearing artificially close.
func hasPCIePaths(gpus []checks.GPUInfo, nics []checks.NICInfo) bool {
	for _, g := range gpus {
		if len(g.PCIePath) >= 2 {
			return true
		}
	}
	for _, n := range nics {
		if len(n.PCIePath) >= 2 {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Pairing strategies
// ---------------------------------------------------------------------------

// buildPairs dispatches to the appropriate pairing strategy based on
// topology shape and GPU:NIC ratio. Returns pairs, isFlat flag, and
// the strategy name.
func buildPairs(gpus []checks.GPUInfo, nics []checks.NICInfo) ([]checks.GPUNICPair, bool, checks.PairingStrategy) {
	if len(nics) == 0 || len(gpus) == 0 {
		return nil, true, checks.PairingNUMAAffinity
	}

	flat := isFlat(gpus, nics)
	if flat || !hasPCIePaths(gpus, nics) {
		return numaAffinityPairing(gpus, nics), flat, checks.PairingNUMAAffinity
	}

	if len(gpus) <= len(nics) {
		return pcieDistancePairing(gpus, nics), false, checks.PairingPCIeDistance
	}

	// GPUs > NICs: load-balance multiple GPUs across fewer NICs
	return numaLoadBalancePairing(gpus, nics), false, checks.PairingNUMALoadBalance
}

// numaAffinityPairing pairs GPUs to NICs by NUMA affinity with ordered matching.
// Within each NUMA group, GPUs are sorted by ID and NICs by device name, then
// zipped 1:1 (with round-robin when GPUs > NICs within a NUMA). Used as the
// fallback strategy for flat topologies or when PCIe paths are unavailable.
func numaAffinityPairing(gpus []checks.GPUInfo, nics []checks.NICInfo) []checks.GPUNICPair {
	gpusByNuma := groupGPUsByNuma(gpus)
	nicsByNuma := groupNICsByNuma(nics)

	for numa := range gpusByNuma {
		sortGPUsByID(gpusByNuma[numa])
	}
	for numa := range nicsByNuma {
		sortNICsByDev(nicsByNuma[numa])
	}

	assignedGPU := make(map[int]bool)
	var pairs []checks.GPUNICPair

	// Phase 1: within-NUMA ordered zip
	for numa, numaGPUs := range gpusByNuma {
		numaNICs := nicsByNuma[numa]
		if len(numaNICs) == 0 {
			continue
		}
		for i, g := range numaGPUs {
			nic := numaNICs[i%len(numaNICs)]
			pairs = append(pairs, makePair(g, nic, 0))
			assignedGPU[g.ID] = true
		}
	}

	// Phase 2: cross-NUMA fallback for GPUs on NUMA nodes with no NICs
	if len(pairs) < len(gpus) && len(nics) > 0 {
		allNICs := sortedNICsCopy(nics)
		fallbackIdx := 0
		for _, g := range gpus {
			if assignedGPU[g.ID] {
				continue
			}
			nic := allNICs[fallbackIdx%len(allNICs)]
			pairs = append(pairs, makePair(g, nic, crossNUMAPenalty))
			fallbackIdx++
		}
	}

	sort.Slice(pairs, func(i, j int) bool { return pairs[i].GPU.ID < pairs[j].GPU.ID })
	return pairs
}

// pcieDistancePairing matches GPUs to NICs 1:1 using PCIe tree distance.
// Phase 1: within each NUMA group, greedily assign by shortest PCIe distance.
// Phase 2: match remaining GPUs and NICs cross-NUMA by PCIe distance.
func pcieDistancePairing(gpus []checks.GPUInfo, nics []checks.NICInfo) []checks.GPUNICPair {
	gpusByNuma := groupGPUsByNuma(gpus)
	nicsByNuma := groupNICsByNuma(nics)

	assignedGPU := make(map[int]bool)
	assignedNIC := make(map[string]bool)
	var pairs []checks.GPUNICPair

	// Phase 1: within-NUMA greedy matching
	for numa, numaGPUs := range gpusByNuma {
		numaNICs := nicsByNuma[numa]
		if len(numaNICs) == 0 {
			continue
		}

		candidates := buildCandidates(numaGPUs, numaNICs)
		for _, c := range candidates {
			if assignedGPU[c.gpu.ID] || assignedNIC[c.nic.Dev] {
				continue
			}
			pairs = append(pairs, makePair(c.gpu, c.nic, c.dist))
			assignedGPU[c.gpu.ID] = true
			assignedNIC[c.nic.Dev] = true
		}
	}

	// Phase 2: cross-NUMA for remaining
	var remainGPUs []checks.GPUInfo
	var remainNICs []checks.NICInfo
	for _, g := range gpus {
		if !assignedGPU[g.ID] {
			remainGPUs = append(remainGPUs, g)
		}
	}
	for _, n := range nics {
		if !assignedNIC[n.Dev] {
			remainNICs = append(remainNICs, n)
		}
	}

	if len(remainGPUs) > 0 && len(remainNICs) > 0 {
		candidates := buildCandidates(remainGPUs, remainNICs)
		for _, c := range candidates {
			if assignedGPU[c.gpu.ID] || assignedNIC[c.nic.Dev] {
				continue
			}
			pairs = append(pairs, makePair(c.gpu, c.nic, c.dist+crossNUMAPenalty))
			assignedGPU[c.gpu.ID] = true
			assignedNIC[c.nic.Dev] = true
		}
	}

	sort.Slice(pairs, func(i, j int) bool { return pairs[i].GPU.ID < pairs[j].GPU.ID })
	return pairs
}

// numaLoadBalancePairing distributes GPUs across NICs within NUMA groups
// when GPU and NIC counts differ. Uses PCIe distance for preference when
// paths are available, with even per-NIC capacity within each NUMA group.
func numaLoadBalancePairing(gpus []checks.GPUInfo, nics []checks.NICInfo) []checks.GPUNICPair {
	gpusByNuma := groupGPUsByNuma(gpus)
	nicsByNuma := groupNICsByNuma(nics)

	assignedGPU := make(map[int]bool)
	var pairs []checks.GPUNICPair

	// Phase 1: within-NUMA load balance
	for numa, numaGPUs := range gpusByNuma {
		numaNICs := nicsByNuma[numa]
		if len(numaNICs) == 0 {
			continue
		}

		sortGPUsByID(numaGPUs)
		sortNICsByDev(numaNICs)

		capacity := (len(numaGPUs) + len(numaNICs) - 1) / len(numaNICs)
		nicLoad := make(map[string]int)

		candidates := buildCandidates(numaGPUs, numaNICs)
		for _, c := range candidates {
			if assignedGPU[c.gpu.ID] || nicLoad[c.nic.Dev] >= capacity {
				continue
			}
			pairs = append(pairs, makePair(c.gpu, c.nic, c.dist))
			assignedGPU[c.gpu.ID] = true
			nicLoad[c.nic.Dev]++
		}
	}

	// Phase 2: cross-NUMA fallback
	if len(pairs) < len(gpus) && len(nics) > 0 {
		allNICs := sortedNICsCopy(nics)
		fallbackIdx := 0
		for _, g := range gpus {
			if assignedGPU[g.ID] {
				continue
			}
			nic := allNICs[fallbackIdx%len(allNICs)]
			var dist int
			if len(g.PCIePath) >= 2 && len(nic.PCIePath) >= 2 {
				dist = pcieDistance(g.PCIePath, nic.PCIePath)
			} else {
				dist = unknownPathPenalty
			}
			pairs = append(pairs, makePair(g, nic, dist+crossNUMAPenalty))
			fallbackIdx++
		}
	}

	sort.Slice(pairs, func(i, j int) bool { return pairs[i].GPU.ID < pairs[j].GPU.ID })
	return pairs
}

// ---------------------------------------------------------------------------
// Shared helpers for pairing strategies
// ---------------------------------------------------------------------------

type pairCandidate struct {
	gpu  checks.GPUInfo
	nic  checks.NICInfo
	dist int
}

// buildCandidates computes all GPU-NIC pair candidates sorted by PCIe
// distance (ascending), with deterministic tie-breaking by GPU ID then
// NIC device name.
func buildCandidates(gpus []checks.GPUInfo, nics []checks.NICInfo) []pairCandidate {
	var candidates []pairCandidate
	for _, g := range gpus {
		for _, n := range nics {
			var dist int
			if len(g.PCIePath) >= 2 && len(n.PCIePath) >= 2 {
				dist = pcieDistance(g.PCIePath, n.PCIePath)
			} else {
				dist = unknownPathPenalty
			}
			candidates = append(candidates, pairCandidate{gpu: g, nic: n, dist: dist})
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].dist != candidates[j].dist {
			return candidates[i].dist < candidates[j].dist
		}
		if candidates[i].gpu.ID != candidates[j].gpu.ID {
			return candidates[i].gpu.ID < candidates[j].gpu.ID
		}
		return candidates[i].nic.Dev < candidates[j].nic.Dev
	})
	return candidates
}

func makePair(g checks.GPUInfo, n checks.NICInfo, hops int) checks.GPUNICPair {
	return checks.GPUNICPair{GPU: g, NIC: n, PCIeHops: hops}
}

func groupGPUsByNuma(gpus []checks.GPUInfo) map[int][]checks.GPUInfo {
	m := make(map[int][]checks.GPUInfo)
	for _, g := range gpus {
		m[g.NUMA] = append(m[g.NUMA], g)
	}
	return m
}

func groupNICsByNuma(nics []checks.NICInfo) map[int][]checks.NICInfo {
	m := make(map[int][]checks.NICInfo)
	for _, n := range nics {
		m[n.NUMA] = append(m[n.NUMA], n)
	}
	return m
}

func sortGPUsByID(gpus []checks.GPUInfo) {
	sort.Slice(gpus, func(i, j int) bool { return gpus[i].ID < gpus[j].ID })
}

func sortNICsByDev(nics []checks.NICInfo) {
	sort.Slice(nics, func(i, j int) bool { return nics[i].Dev < nics[j].Dev })
}

func sortedNICsCopy(nics []checks.NICInfo) []checks.NICInfo {
	cp := make([]checks.NICInfo, len(nics))
	copy(cp, nics)
	sortNICsByDev(cp)
	return cp
}
