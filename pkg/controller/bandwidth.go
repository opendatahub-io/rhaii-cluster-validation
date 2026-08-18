package controller

import (
	"context"
	"fmt"
	"strings"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks/rdma"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/config"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/jobrunner"
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

	return jobs, nil
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
