package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks/rdma"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/config"
)

// runGPUCheckPhase deploys and collects per-node GPU check Jobs (Run Step 6).
// Only a deploy failure is fatal; collection errors are logged as warnings so
// the run can still produce a (partial) report.
func (c *Controller) runGPUCheckPhase(ctx context.Context) ([]checks.NodeReport, error) {
	fmt.Fprintln(c.output, "[Step 6] Deploying per-node GPU check Jobs...")
	if err := c.deployNodeCheckJobs(ctx, nodeCheckJobSpec{
		kind:        "GPU check",
		namePrefix:  "rhaii-validate-check-",
		labelValue:  gpuCheckJobLabelValue,
		resourceCfg: c.cfg.Agent,
		checkMode:   CheckModeGPU,
	}); err != nil {
		return nil, fmt.Errorf("failed to deploy GPU check jobs: %w", err)
	}

	fmt.Fprintln(c.output, "  Waiting for GPU check Jobs to complete...")
	gpuReports, err := c.waitAndCollectGpuCheckJobs(ctx)
	if err != nil {
		fmt.Fprintf(c.output, "  Warning: GPU check collection error: %v\n", err)
	}

	if !c.opts.Debug {
		c.cleanupGpuCheckJobs(ctx)
	}
	return gpuReports, nil
}

// runRDMANodeCheckPhase deploys and collects per-node RDMA node check Jobs
// (topology + devices + NIC status), and runs the intra-host bandwidth probe
// to pair GPUs with NICs when a flat PCIe topology is detected (Run Step 7).
// Only a deploy failure is fatal; collection/probe errors are logged as
// warnings so the run can still produce a (partial) report.
func (c *Controller) runRDMANodeCheckPhase(ctx context.Context) ([]checks.NodeReport, error) {
	fmt.Fprintln(c.output, "[Step 7] Deploying per-node RDMA node check Jobs...")
	if err := c.deployNodeCheckJobs(ctx, nodeCheckJobSpec{
		kind:        "RDMA node check",
		namePrefix:  "rhaii-validate-net-",
		labelValue:  netCheckJobLabelValue,
		resourceCfg: c.cfg.Jobs,
		checkMode:   CheckModeRDMANode,
		setRDMAType: true,
	}); err != nil {
		return nil, fmt.Errorf("failed to deploy RDMA node check jobs: %w", err)
	}

	fmt.Fprintln(c.output, "  Waiting for RDMA node check Jobs to complete...")
	netReports, err := c.waitAndCollectNetCheckJobs(ctx)
	if err != nil {
		fmt.Fprintf(c.output, "  Warning: RDMA node check collection error: %v\n", err)
	}

	// Flat topology detected — net-check is incomplete without BW probe pairing.
	// --debug keeps whichever pods are "final": net-check if flat=false, BW probe if flat=true.
	if rdma.NeedsBandwidthProbe(netReports) {
		if c.gpuVendor == config.GPUVendorAMD {
			fmt.Fprintln(c.output, "  BW probe skipped: AMD GPUs not supported by tools image")
			if !c.opts.Debug {
				c.cleanupNetCheckJobs(ctx)
			}
		} else {
			// All net-check reports are already collected in memory. Clean up
			// net-check jobs cluster-wide to free GPU/RDMA device resources
			// for the BW probe (which needs all GPUs on each flat node).
			if !c.cleanupNetCheckJobs(ctx) {
				fmt.Fprintln(c.output, "  Waiting for net-check pods to fully terminate...")
				c.waitForPodsGone(checkJobLabelKey+"="+netCheckJobLabelValue, 60*time.Second)
			}
			fmt.Fprintln(c.output, "  Flat PCIe topology detected. Running GPU-NIC pairwise intra-host bandwidth tests...")
			fmt.Fprintln(c.output, "  This may take ~10 minutes per node (testing all GPU-NIC combinations).")
			if probeErr := c.deployLoopbackBWProbeJobs(ctx, netReports); probeErr != nil {
				fmt.Fprintf(c.output, "  Warning: failed to deploy BW probe jobs: %v\n", probeErr)
			} else if c.bwProbeMaxMatrixSize == 0 {
				fmt.Fprintln(c.output, "  No BW probe jobs created (all flat nodes skipped)")
			} else {
				bwResults, probeErr := c.waitAndCollectLoopbackBWProbeJobs(ctx)
				if probeErr != nil {
					fmt.Fprintf(c.output, "  Warning: BW probe collection error: %v\n", probeErr)
				}
				if len(bwResults) > 0 {
					netReports = rdma.ApplyBandwidthPairing(netReports, bwResults, c.output)
				}
				if !c.opts.Debug {
					c.cleanupLoopbackBWProbeJobs(ctx)
				}
			}
		}
		// Mark flat nodes that still have NUMA-affinity pairing as WARN.
		// This covers BW probe failures (NVIDIA) and unsupported vendors (AMD).
		netReports = rdma.WarnUnpairedFlatNodes(netReports)
	} else if !c.opts.Debug {
		c.cleanupNetCheckJobs(ctx)
	}

	return netReports, nil
}
