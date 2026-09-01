package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks/rdma"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/config"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/jobrunner"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (c *Controller) runBandwidthJobs(ctx context.Context, gpuNodes []string, reports []checks.NodeReport) ([]jobrunner.JobResult, error) {
	if len(c.jobs) == 0 {
		fmt.Fprintf(c.output, "  No jobs registered, skipping bandwidth tests\n")
		return nil, nil
	}
	if c.gpuVendor == config.GPUVendorAMD {
		fmt.Fprintf(c.output, "  AMD GPU detected, skipping bandwidth jobs (NVIDIA-only images)\n")
		return nil, nil
	}
	if len(gpuNodes) < 2 {
		return nil, fmt.Errorf("need at least 2 GPU nodes for bandwidth tests (have %d)", len(gpuNodes))
	}

	c.configureJobs(ctx, gpuNodes)

	// Build topology map from node reports
	topoMap := rdma.BuildTopologyMap(reports)
	if len(topoMap) > 0 {
		fmt.Fprintf(c.output, "  Topology available for %d node(s)\n", len(topoMap))
	}

	// Expand RDMA jobs: one per GPU-NIC pair from topology
	jobs, skipResults := c.expandRDMAJobs(ctx, gpuNodes, topoMap, reports)

	runner := jobrunner.New(c.client, c.opts.Namespace, c.opts.Image, c.opts.Timeout, c.output, c.opts.Debug)

	var results []jobrunner.JobResult
	results = append(results, skipResults...)

	// User-specified nodes: star topology (1 server, N clients)
	if c.opts.ServerNode != "" || len(c.opts.ClientNodes) > 0 {
		serverNode, clientNodes := c.resolveStarNodes(gpuNodes)
		jr, err := runner.RunStar(ctx, jobs, serverNode, clientNodes)
		return append(results, jr...), err
	}

	// Default: ring topology (every node tested as both server and client)
	jr, err := runner.RunRing(ctx, jobs, gpuNodes)
	return append(results, jr...), err
}

// expandRDMAJobs creates per-GPU-NIC RDMA jobs from topology.
// For iperf3 (TCP), topology doesn't matter — keep one job.
// For ib-write-bw, create one job per GPU-NIC pair so every NIC is tested.
func (c *Controller) expandRDMAJobs(ctx context.Context, gpuNodes []string, topoMap map[string]*checks.NodeTopology, reports []checks.NodeReport) ([]jobrunner.Job, []jobrunner.JobResult) {
	if len(topoMap) == 0 {
		return c.jobs, nil
	}

	// Find the first topology (all nodes should have same GPU count)
	var topo *checks.NodeTopology
	for _, t := range topoMap {
		topo = t
		break
	}

	// RDMA availability is determined by topology: if net-checks found
	// RDMA NICs paired with GPUs, RDMA tests should run. The RDMA resource
	// request (rdma/shared_ib, rdma/ib, etc.) is can be a boolean flag for device
	// plugin access, not a device count.
	rdmaAvailable := len(topo.Pairs) > 0

	var jobs []jobrunner.Job
	var skipResults []jobrunner.JobResult
	for _, job := range c.jobs {
		if job.Name() != "ib-write-bw" {
			jobs = append(jobs, job)
			continue
		}

		if !rdmaAvailable {
			fmt.Fprintf(c.output, "  Skipping RDMA jobs: no RDMA NICs found in topology\n")
			return jobs, []jobrunner.JobResult{{
				JobName: "ib-write-bw",
				Status:  checks.StatusSkip,
				Message: "RDMA skipped: no RDMA NICs found in GPU-NIC topology (run rdma-node first)",
			}}
		}

		var origPodCfg *jobrunner.PodConfig
		var origServerImg, origClientImg string
		if orig, ok := job.(*rdma.RDMABandwidthJob); ok {
			origPodCfg = orig.PodCfg
			origServerImg = orig.ServerImage
			origClientImg = orig.ClientImage
		}

		// Resolve the fabric from only the GPU nodes participating in this run
		// The registered ib-write-bw job is the common RDMA bandwidth placeholder
		// SRD replaces it with EFA jobs while IB/RoCE continues through the existing path
		selectedTopo := make(map[string]*checks.NodeTopology, len(gpuNodes))
		for _, node := range gpuNodes {
			if selected, ok := topoMap[node]; ok {
				selectedTopo[node] = selected
			}
		}
		rdmaType, err := rdma.ResolveRDMAType(c.cfg.Jobs.RDMAType, selectedTopo)
		if err != nil {
			skipResults = append(skipResults, jobrunner.JobResult{
				JobName: "ib-write-bw",
				Status:  checks.StatusSkip,
				Message: fmt.Sprintf("RDMA bandwidth skipped: cannot resolve fabric type: %v", err),
			})
			continue
		}

		if rdmaType == config.RDMATypeSRD {
			efaJobs, efaSkips := c.expandEFABandwidthJobs(ctx, gpuNodes, selectedTopo, origPodCfg, origServerImg, origClientImg)
			jobs = append(jobs, efaJobs...)
			skipResults = append(skipResults, efaSkips...)
			continue
		}

		// GPUDirect RDMA: request all GPUs so the NVIDIA container runtime
		// injects CUDA libraries and --use_cuda sees correct GPU indices
		if c.gpuResource != "" && topo.GPUCount > 0 && origPodCfg != nil {
			gpuCountStr := fmt.Sprintf("%d", topo.GPUCount)
			origPodCfg.ResourceRequests[string(c.gpuResource)] = gpuCountStr
			origPodCfg.ResourceLimits[string(c.gpuResource)] = gpuCountStr
		}

		// Collect unique RDMA devices for the WEP (whole-endpoint) job.
		// Multiple GPUs may share the same NIC (e.g. GPU0↔mlx5_0, GPU1↔mlx5_0),
		// so we deduplicate to avoid running ib_write_bw on the same NIC twice.
		var rdmaDevices []string
		var gpuIDs []int
		uniqueDevices := make(map[string]bool)

		cfgQPs := c.cfg.Jobs.RDMA.QPs
		cfgMsgSize := c.cfg.Jobs.RDMA.MessageSize

		// Create one PD job per GPU-NIC pair from topology
		for _, pair := range topo.Pairs {
			rdmaJob := rdma.NewRDMABandwidthJob(c.cfg.Thresholds.RDMABandwidthPD.Pass, c.cfg.Thresholds.RDMABandwidthPD.Warn, nil)
			rdmaJob.PodCfg = origPodCfg.Clone()
			rdmaJob.ServerImage = origServerImg
			rdmaJob.ClientImage = origClientImg
			rdmaJob.Device = pair.NIC.Dev
			rdmaJob.UseCUDA = pair.GPU.ID
			if cfgQPs > 0 {
				rdmaJob.QPs = cfgQPs
			}
			if cfgMsgSize > 0 {
				rdmaJob.MessageSize = cfgMsgSize
			}
			jobs = append(jobs, rdmaJob)
			fmt.Fprintf(c.output, "  RDMA PD job: GPU%d ↔ %s (NUMA:%d↔%d)\n", pair.GPU.ID, pair.NIC.Dev, pair.GPU.NUMA, pair.NIC.NUMA)

			if !uniqueDevices[pair.NIC.Dev] {
				rdmaDevices = append(rdmaDevices, pair.NIC.Dev)
				gpuIDs = append(gpuIDs, pair.GPU.ID)
				uniqueDevices[pair.NIC.Dev] = true
			}
		}

		// Add WEP job if multiple NICs available
		if len(rdmaDevices) > 1 {
			wepJob := rdma.NewRDMAWEPJob(c.cfg.Thresholds.RDMABandwidthWEP.Pass, c.cfg.Thresholds.RDMABandwidthWEP.Warn, rdmaDevices, gpuIDs)
			wepJob.PodCfg = origPodCfg.Clone()
			wepJob.ServerImage = origServerImg
			wepJob.ClientImage = origClientImg
			if cfgQPs > 0 {
				wepJob.QPs = cfgQPs
			}
			if cfgMsgSize > 0 {
				wepJob.MessageSize = cfgMsgSize
			}
			jobs = append(jobs, wepJob)
			fmt.Fprintf(c.output, "  RDMA WEP job: %d NICs in parallel (%s)\n", len(rdmaDevices), strings.Join(rdmaDevices, ", "))
		} else {
			fmt.Fprintf(c.output, "  RDMA WEP skipped: only %d NIC(s), need 2+ for whole-endpoint test\n", len(rdmaDevices))
		}
	}

	return jobs, skipResults
}

// expandEFABandwidthJobs converts each node's EFA topology into bandwidth jobs
// First it groups PCIe-aligned NICs by GPU on every selected node
// It then creates one isolated PD job per compatible GPU group and finally one
// WEP job containing all compatible groups in a stable order
func (c *Controller) expandEFABandwidthJobs(ctx context.Context, gpuNodes []string, topoMap map[string]*checks.NodeTopology, basePodCfg *jobrunner.PodConfig, serverImage, clientImage string) ([]jobrunner.Job, []jobrunner.JobResult) {
	// groupsByNode[node][gpuID] is the NIC/GPU lane group used to build both
	// endpoints of that GPU's PD job later in this function
	groupsByNode := make(map[string]map[int][]rdma.EFABandwidthLane, len(gpuNodes))
	// Each pod requests all GPUs and EFAs so CUDA indices and EFA interfaces
	// remain visible even though a PD job exercises only one aligned group
	podCfgByNode := make(map[string]*jobrunner.PodConfig, len(gpuNodes))
	// The union of GPU IDs provides deterministic PD and WEP construction order
	gpuIDs := make(map[int]bool)

	// Build and validate the per-node GPU groups from stored topology
	for _, nodeName := range gpuNodes {
		topo := topoMap[nodeName]
		if topo == nil {
			return nil, []jobrunner.JobResult{{JobName: "efa-rma-bw", Status: checks.StatusSkip, Message: fmt.Sprintf("EFA bandwidth skipped: no topology for node %s", nodeName)}}
		}

		groups := make(map[int][]rdma.EFABandwidthLane)
		seenDevices := make(map[string]bool)
		for _, pair := range topo.Pairs {
			if pair.GPU.ID < 0 {
				return nil, []jobrunner.JobResult{{JobName: "efa-rma-bw", Status: checks.StatusSkip, Message: fmt.Sprintf("EFA bandwidth skipped: invalid GPU ID %d on node %s", pair.GPU.ID, nodeName)}}
			}
			if !checks.ValidDeviceName.MatchString(pair.NIC.Dev) {
				return nil, []jobrunner.JobResult{{JobName: "efa-rma-bw", Status: checks.StatusSkip, Message: fmt.Sprintf("EFA bandwidth skipped: invalid device %q on node %s", pair.NIC.Dev, nodeName)}}
			}
			if seenDevices[pair.NIC.Dev] {
				return nil, []jobrunner.JobResult{{JobName: "efa-rma-bw", Status: checks.StatusSkip, Message: fmt.Sprintf("EFA bandwidth skipped: device %s is assigned more than once on node %s", pair.NIC.Dev, nodeName)}}
			}
			seenDevices[pair.NIC.Dev] = true
			groups[pair.GPU.ID] = append(groups[pair.GPU.ID], rdma.EFABandwidthLane{GPUID: pair.GPU.ID, Device: pair.NIC.Dev})
			gpuIDs[pair.GPU.ID] = true
		}
		if len(seenDevices) == 0 {
			return nil, []jobrunner.JobResult{{JobName: "efa-rma-bw", Status: checks.StatusSkip, Message: fmt.Sprintf("EFA bandwidth skipped: no GPU-paired EFA devices on node %s", nodeName)}}
		}
		for gpuID := range groups {
			sort.Slice(groups[gpuID], func(i, j int) bool { return groups[gpuID][i].Device < groups[gpuID][j].Device })
		}
		groupsByNode[nodeName] = groups

		node, err := c.client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return nil, []jobrunner.JobResult{{JobName: "efa-rma-bw", Status: checks.StatusSkip, Message: fmt.Sprintf("EFA bandwidth skipped: failed to get node %s for EFA count: %v", nodeName, err)}}
		}
		efaCount := config.AutoEFACount(node.Status.Allocatable, config.ResourceConfigHasEFA(c.cfg.Jobs), c.cfg.Jobs.GetEFACount())
		if efaCount < int64(len(seenDevices)) {
			return nil, []jobrunner.JobResult{{JobName: "efa-rma-bw", Status: checks.StatusSkip, Message: fmt.Sprintf("EFA bandwidth skipped: node %s requires %d EFA devices from topology but resource injection resolved %d", nodeName, len(seenDevices), efaCount)}}
		}
		gpuCount := c.gpuCounts[nodeName]
		if gpuCount <= 0 {
			return nil, []jobrunner.JobResult{{JobName: "efa-rma-bw", Status: checks.StatusSkip, Message: fmt.Sprintf("EFA bandwidth skipped: no allocatable GPU count for node %s", nodeName)}}
		}
		if c.gpuResource == "" {
			return nil, []jobrunner.JobResult{{JobName: "efa-rma-bw", Status: checks.StatusSkip, Message: "EFA bandwidth skipped: GPU resource name is unavailable"}}
		}

		// Resource counts are node-specific but intentionally include every device
		podCfg := basePodCfg.Clone()
		if podCfg == nil {
			podCfg = &jobrunner.PodConfig{}
		}
		if podCfg.ResourceRequests == nil {
			podCfg.ResourceRequests = make(map[string]string)
		}
		if podCfg.ResourceLimits == nil {
			podCfg.ResourceLimits = make(map[string]string)
		}
		podCfg.Privileged = false
		podCfg.ResourceRequests[string(c.gpuResource)] = fmt.Sprintf("%d", gpuCount)
		podCfg.ResourceLimits[string(c.gpuResource)] = fmt.Sprintf("%d", gpuCount)
		podCfg.ResourceRequests[string(config.EFAResourceName)] = fmt.Sprintf("%d", efaCount)
		podCfg.ResourceLimits[string(config.EFAResourceName)] = fmt.Sprintf("%d", efaCount)
		podCfgByNode[nodeName] = podCfg
	}

	// Stable GPU ordering keeps PD execution and WEP lane-to-port assignment deterministic
	sortedGPUIDs := make([]int, 0, len(gpuIDs))
	for gpuID := range gpuIDs {
		sortedGPUIDs = append(sortedGPUIDs, gpuID)
	}
	sort.Ints(sortedGPUIDs)

	// Create one PD job per GPU only when every selected node has that group with
	// the same lane count so server and client start matching fi_rma_bw processes
	messageSize := c.cfg.Jobs.RDMA.MessageSize
	var jobs []jobrunner.Job
	var skips []jobrunner.JobResult
	allGroupsCompatible := true
	for _, gpuID := range sortedGPUIDs {
		lanesByNode := make(map[string][]rdma.EFABandwidthLane, len(gpuNodes))
		width := -1
		compatible := true
		for _, nodeName := range gpuNodes {
			lanes := groupsByNode[nodeName][gpuID]
			if len(lanes) == 0 || (width >= 0 && len(lanes) != width) {
				compatible = false
				break
			}
			width = len(lanes)
			lanesByNode[nodeName] = lanes
		}
		if !compatible {
			// A missing or differently sized group cannot form matching endpoints
			// WEP is also unsafe because it is assembled from all PD groups below
			allGroupsCompatible = false
			skips = append(skips, jobrunner.JobResult{JobName: fmt.Sprintf("efa-rma-bw-gpu%d", gpuID), Status: checks.StatusSkip, Message: fmt.Sprintf("EFA GPU%d bandwidth skipped: group missing or NIC count differs across selected nodes", gpuID)})
			continue
		}

		job := rdma.NewEFABandwidthJob(gpuID, false, c.cfg.Thresholds.RDMABandwidthPD.Pass, c.cfg.Thresholds.RDMABandwidthPD.Warn, lanesByNode, podCfgByNode)
		if messageSize > 0 {
			job.MessageSize = messageSize
		}
		job.ServerImage = serverImage
		job.ClientImage = clientImage
		jobs = append(jobs, job)
		fmt.Fprintf(c.output, "  EFA PD job: GPU%d with %d NIC(s) per node\n", gpuID, width)
	}

	if !allGroupsCompatible {
		skips = append(skips, jobrunner.JobResult{JobName: "efa-rma-bw-wep", Status: checks.StatusSkip, Message: "EFA WEP bandwidth skipped: GPU-NIC groups differ across selected nodes"})
		return jobs, skips
	}

	// WEP requires the complete group layout to match across all selected nodes
	// Flattening in sorted GPU order gives both endpoints identical lane slots and
	// therefore identical per-lane ports while all endpoint NICs run together
	wepLanesByNode := make(map[string][]rdma.EFABandwidthLane, len(gpuNodes))
	for _, nodeName := range gpuNodes {
		for _, gpuID := range sortedGPUIDs {
			wepLanesByNode[nodeName] = append(wepLanesByNode[nodeName], groupsByNode[nodeName][gpuID]...)
		}
	}
	if len(wepLanesByNode[gpuNodes[0]]) > 1 {
		wepJob := rdma.NewEFABandwidthJob(-1, true, c.cfg.Thresholds.RDMABandwidthWEP.Pass, c.cfg.Thresholds.RDMABandwidthWEP.Warn, wepLanesByNode, podCfgByNode)
		if messageSize > 0 {
			wepJob.MessageSize = messageSize
		}
		wepJob.ServerImage = serverImage
		wepJob.ClientImage = clientImage
		jobs = append(jobs, wepJob)
		fmt.Fprintf(c.output, "  EFA WEP job: %d NIC(s) per node in parallel\n", len(wepLanesByNode[gpuNodes[0]]))
	} else {
		skips = append(skips, jobrunner.JobResult{JobName: "efa-rma-bw-wep", Status: checks.StatusSkip, Message: "EFA WEP bandwidth skipped: need at least two NICs"})
	}

	return jobs, skips
}

// configureJobs applies GPU resources, thresholds, and images to all registered jobs.
func (c *Controller) configureJobs(ctx context.Context, gpuNodes []string) {
	// Split config: TCP jobs get only cpu/memory, RDMA jobs get everything
	tcpCfg := &jobrunner.PodConfig{
		ResourceRequests: make(map[string]string),
		ResourceLimits:   make(map[string]string),
		Annotations:      make(map[string]string),
	}
	rdmaCfg := &jobrunner.PodConfig{
		ResourceRequests: make(map[string]string),
		ResourceLimits:   make(map[string]string),
		Annotations:      make(map[string]string),
	}

	for k, v := range c.cfg.Jobs.Requests {
		rdmaCfg.ResourceRequests[k] = v
		if k == "cpu" || k == "memory" {
			tcpCfg.ResourceRequests[k] = v
		}
	}
	for k, v := range c.cfg.Jobs.Limits {
		rdmaCfg.ResourceLimits[k] = v
		if k == "cpu" || k == "memory" {
			tcpCfg.ResourceLimits[k] = v
		}
	}
	for k, v := range c.cfg.Jobs.Annotations {
		tcpCfg.Annotations[k] = v
		rdmaCfg.Annotations[k] = v
	}

	for _, job := range c.jobs {
		// Pod config: TCP jobs get only cpu/memory, RDMA jobs get everything
		if configurable, ok := job.(jobrunner.Configurable); ok {
			if strings.HasPrefix(job.Name(), "ib-") {
				configurable.SetPodConfig(rdmaCfg)
			} else {
				configurable.SetPodConfig(tcpCfg)
			}
		}

		// Thresholds from platform config
		if tc, ok := job.(jobrunner.ThresholdConfigurable); ok {
			switch job.Name() {
			case "iperf3-tcp":
				tc.SetThreshold(c.cfg.Thresholds.TCPBandwidth.Pass, c.cfg.Thresholds.TCPBandwidth.Warn)
			case "tcp-latency":
				tc.SetThreshold(c.cfg.Thresholds.TCPLatency.Pass, c.cfg.Thresholds.TCPLatency.Warn)
			case "ib-write-bw":
				tc.SetThreshold(c.cfg.Thresholds.RDMABandwidthPD.Pass, c.cfg.Thresholds.RDMABandwidthPD.Warn)
			}
		}

		// Container images: tcp-latency uses validator image (built-in tcp-lat),
		// all other jobs use the tools image.
		if imgConfig, ok := job.(jobrunner.ImageConfigurable); ok {
			jobImage := c.opts.ToolsImage
			if job.Name() == "tcp-latency" {
				jobImage = c.opts.Image
			}

			if imgConfig.GetServerImage() == "" {
				if setter, ok := job.(interface{ SetServerImage(string) }); ok {
					setter.SetServerImage(jobImage)
				}
			}
			if imgConfig.GetClientImage() == "" {
				if setter, ok := job.(interface{ SetClientImage(string) }); ok {
					setter.SetClientImage(jobImage)
				}
			}
			fmt.Fprintf(c.output, "  Job %s: using image %s\n", job.Name(), jobImage)
		}
	}
}

// resolveStarNodes returns the server and client nodes for star topology.
func (c *Controller) resolveStarNodes(gpuNodes []string) (string, []string) {
	serverNode := c.opts.ServerNode
	clientNodes := c.opts.ClientNodes
	if serverNode == "" {
		serverNode = gpuNodes[0]
	}
	if len(clientNodes) == 0 {
		for _, n := range gpuNodes {
			if n != serverNode {
				clientNodes = append(clientNodes, n)
			}
		}
	}
	return serverNode, clientNodes
}
