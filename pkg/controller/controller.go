package controller

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/opendatahub-io/rhaii-cluster-validation/deploy"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks/rdma"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/config"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/jobrunner"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	checkJobLabelKey      = "app"
	gpuCheckJobLabelValue = "rhaii-validate-gpu-check"
	netCheckJobLabelValue = "rhaii-validate-net-check"
	bwProbeLabelValue     = "rhaii-validate-bw-probe"
	configMapName         = "rhaii-validate-config"

	// BW probe per-test time budget components (seconds).
	// Each GPU-NIC pair runs: server startup + ib_write_bw test + teardown overhead.
	bwProbeServerStartupSecs = 2
	bwProbePerTestSecs       = 30 // matches DefaultPerTestTimeoutSecs
	bwProbeOverheadSecs      = 2
	bwProbePerPairBudgetSecs = bwProbeServerStartupSecs + bwProbePerTestSecs + bwProbeOverheadSecs // ~34s
	bwProbeMinTimeoutSecs    = 900                                                                 // 15-minute floor
	reportCMName             = "rhaii-validate-report"
	pingmeshFailuresCMName   = "rhaii-validate-pingmesh-failures"
	defaultTimeout           = 5 * time.Minute
)

// CheckMode constants define the validation modes used by both the CLI
// subcommands and the internal per-node Job pods (via CHECK_MODE env var).
const (
	CheckModeGPU           = "gpu"
	CheckModeNetwork       = "network"
	CheckModeRDMA          = "rdma"
	CheckModeRDMANode      = "rdma-node"
	CheckModeRDMAPing      = "rdma-ping"
	CheckModeRDMABandwidth = "rdma-bandwidth"
	CheckModeDeps          = "deps"
	CheckModeAll           = "all"
)

// Options configures the controller behavior.
type Options struct {
	Kubeconfig   string
	Namespace    string
	Image        string // Validator container image (self-reference)
	ToolsImage   string // Tools container image (iperf3, RDMA, pingmesh)
	Timeout      time.Duration
	ConfigFile   string
	Nodes        []string // Restrict to specific nodes (default: all GPU nodes)
	ServerNode   string
	ClientNodes  []string
	Debug        bool   // Skip cleanup so user can exec into pods for debugging
	OutputFormat string // "table" (default) or "json"
	CheckMode    string // "all", "gpu", "network", "rdma", "rdma-node", "rdma-ping", "rdma-bandwidth", "deps"
	PullSecret   string // Name of an existing image pull secret to attach to the SA
}

// Controller orchestrates check job deployment, result collection, and cleanup.
type Controller struct {
	client               kubernetes.Interface
	opts                 Options
	cfg                  config.PlatformConfig
	output               io.Writer
	platform             config.Platform
	gpuVendor            config.GPUVendor    // auto-detected from node labels
	gpuNodeLabel         string              // label used to discover GPU nodes (empty = fallback to resources)
	gpuNodes             []string            // discovered GPU node names
	gpuCounts            map[string]int64    // GPU count per node (from allocatable)
	efaCounts            map[string]int64    // EFA count per node on EKS (from allocatable)
	gpuResource          corev1.ResourceName // e.g. "nvidia.com/gpu" or "amd.com/gpu"
	jobs                 []jobrunner.Job
	clusterResults       []checks.Result      // Tier 1 (API) check results (CRDs, etc.)
	pingmeshReport       *rdma.PingMeshReport // populated by runPingMesh
	reportStored         bool                 // true after storeReport succeeds
	bwProbeMaxMatrixSize int                  // largest GPU×NIC matrix across deployed BW probe jobs
}

// AddJob registers a multi-node job to run when --bandwidth is enabled.
func (c *Controller) AddJob(j jobrunner.Job) {
	c.jobs = append(c.jobs, j)
}

// Cleanup removes all validation resources from the cluster.
func (c *Controller) Cleanup() error {
	ctx := context.Background()
	fmt.Fprintln(c.output, "Cleaning up all validation resources...")

	// Delete pingmesh failures ConfigMap (explicit clean removes everything)
	if err := c.client.CoreV1().ConfigMaps(c.opts.Namespace).Delete(ctx, pingmeshFailuresCMName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		fmt.Fprintf(c.output, "  Warning: failed to delete %s: %v\n", pingmeshFailuresCMName, err)
	}

	if err := c.cleanupAll(ctx); err != nil {
		return fmt.Errorf("cleanup failed: %w", err)
	}
	fmt.Fprintln(c.output, "Done")
	return nil
}

// New creates a new Controller.
func New(opts Options, output io.Writer) (*Controller, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if opts.Kubeconfig != "" {
		loadingRules.ExplicitPath = opts.Kubeconfig
	}
	configOverrides := &clientcmd.ConfigOverrides{}
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)

	restConfig, err := kubeConfig.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to build kubeconfig: %w", err)
	}

	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes client: %w", err)
	}

	if opts.Namespace == "" {
		opts.Namespace = "rhaii-validation"
	}
	if opts.Timeout == 0 {
		opts.Timeout = defaultTimeout
	}

	return &Controller{
		client: client,
		opts:   opts,
		output: output,
	}, nil
}

// Run executes the full validation lifecycle.
func (c *Controller) Run(ctx context.Context) error {
	fmt.Fprintln(c.output, "=== RHAII Cluster Validation ===")
	fmt.Fprintln(c.output)

	// Step 1: Cleanup previous runs (GPU check + net check + BW probe + bandwidth + pingmesh jobs)
	fmt.Fprintln(c.output, "[Step 1] Cleaning up previous runs...")
	c.cleanupGpuCheckJobs(ctx)
	c.cleanupNetCheckJobs(ctx)
	c.cleanupLoopbackBWProbeJobs(ctx)
	c.cleanupBandwidthJobs(ctx)
	c.cleanupPingMeshJobs(ctx)

	// Step 2: Ensure namespace exists
	fmt.Fprintln(c.output, "[Step 2] Ensuring namespace exists...")
	if err := c.ensureNamespace(ctx); err != nil {
		return fmt.Errorf("failed to create namespace: %w", err)
	}

	// Step 3: Ensure RBAC (ServiceAccount, ClusterRole, ClusterRoleBinding)
	fmt.Fprintln(c.output, "[Step 3] Ensuring RBAC...")
	if err := deploy.EnsureRBAC(ctx, c.client, c.opts.Namespace, c.opts.PullSecret, c.output); err != nil {
		return fmt.Errorf("failed to create RBAC: %w", err)
	}

	// Ensure cluster-scoped RBAC is cleaned up on any early return.
	// In --debug mode we skip cleanup but print the exact commands to run manually.
	defer func() {
		if c.opts.Debug {
			c.printDebugCleanupHint()
			return
		}
		if err := c.cleanupAll(context.Background()); err != nil {
			fmt.Fprintf(c.output, "  Warning: deferred cleanup failed: %v\n", err)
		}
	}()

	// Step 4: Detect platform and create config ConfigMap
	fmt.Fprintln(c.output, "[Step 4] Detecting platform and creating config...")
	if err := c.detectAndCreateConfig(ctx); err != nil {
		return fmt.Errorf("failed to create platform config: %w", err)
	}

	// OpenShift: grant privileged SCC (needed for host sysfs access in topology checks)
	if c.platform == config.PlatformOCP {
		if err := deploy.EnsureOpenShiftSCC(ctx, c.client, c.opts.Namespace); err != nil {
			fmt.Fprintf(c.output, "  Warning: failed to create SCC binding: %v\n", err)
		} else {
			fmt.Fprintln(c.output, "  OpenShift: granted privileged SCC to rhaii-validator")
		}
	}

	// Step 5: Tier 1 checks (CRDs + operator health)
	if c.opts.CheckMode == CheckModeAll || c.opts.CheckMode == CheckModeDeps {
		fmt.Fprintln(c.output, "[Step 5] Checking required CRDs...")
		c.clusterResults = c.RunCRDChecks(ctx)
		for _, r := range c.clusterResults {
			fmt.Fprintf(c.output, "  [%s] %s: %s\n", r.Status, r.Name, r.Message)
		}

		fmt.Fprintln(c.output, "[Step 5b] Checking operator health...")
		operatorResults := c.RunOperatorChecks(ctx)
		c.clusterResults = append(c.clusterResults, operatorResults...)
		for _, r := range operatorResults {
			fmt.Fprintf(c.output, "  [%s] %s: %s\n", r.Status, r.Name, r.Message)
		}
	}

	// Step 6: Discover GPU nodes
	fmt.Fprintln(c.output, "[Step 6] Discovering GPU nodes...")
	gpuNodes, err := c.discoverGPUNodes(ctx)
	if err != nil {
		return fmt.Errorf("failed to discover GPU nodes: %w", err)
	}
	c.gpuNodes = gpuNodes
	if len(gpuNodes) == 0 {
		fmt.Fprintln(c.output, "  No GPU nodes found.")

		// Still report Tier 1 results (CRD checks) even without GPU nodes
		if len(c.clusterResults) > 0 {
			if c.opts.OutputFormat == "json" {
				c.printJSONReport(nil, nil)
			} else {
				c.printReport(nil, nil)
			}
		}

		hasCRDFailures := false
		for _, r := range c.clusterResults {
			if r.Status == checks.StatusFail {
				hasCRDFailures = true
				break
			}
		}
		if hasCRDFailures {
			return fmt.Errorf("validation failed: one or more dependency checks reported FAIL")
		}
		return nil
	}
	fmt.Fprintf(c.output, "  Found %d GPU node(s): %s\n", len(gpuNodes), strings.Join(gpuNodes, ", "))
	for _, name := range gpuNodes {
		if count, ok := c.gpuCounts[name]; ok {
			fmt.Fprintf(c.output, "    %s: %d GPU(s) [%s]\n", name, count, c.gpuResource)
		}
	}

	// Step 6: Deploy per-node GPU check Jobs
	var gpuReports []checks.NodeReport
	needGpuChecks := c.opts.CheckMode == CheckModeGPU || c.opts.CheckMode == CheckModeAll
	if needGpuChecks {
		gpuReports, err = c.runGPUCheckPhase(ctx)
		if err != nil {
			return err
		}
	}

	// Step 7: Deploy per-node RDMA node check Jobs (topology + devices + NIC status),
	// including the intra-host bandwidth probe for flat-topology nodes.
	var netReports []checks.NodeReport
	needNetChecks := c.opts.CheckMode == CheckModeRDMA || c.opts.CheckMode == CheckModeRDMANode || c.opts.CheckMode == CheckModeAll
	if needNetChecks {
		netReports, err = c.runRDMANodeCheckPhase(ctx)
		if err != nil {
			return err
		}
	}

	// Step 8: Run pingmesh RDMA connectivity test
	needPingMesh := c.opts.CheckMode == CheckModeRDMA || c.opts.CheckMode == CheckModeRDMAPing || c.opts.CheckMode == CheckModeAll
	if needPingMesh && len(gpuNodes) >= 2 {
		fmt.Fprintln(c.output, "[Step 8] Running RDMA connectivity mesh (PingMesh)...")
		pmNetReports := netReports
		if !needNetChecks || len(pmNetReports) == 0 {
			// rdma-node didn't run this session — load topology from stored report
			stored, topoErr := c.loadTopologyFromReport(ctx, gpuNodes)
			if topoErr != nil {
				fmt.Fprintf(c.output, "  Warning: %v\n", topoErr)
				fmt.Fprintln(c.output, "  Hint: run 'kubectl rhaii-validate rdma-node' first to generate topology")
				c.pingmeshReport = rdma.SkipPingMeshReport("Skipped: topology incomplete for all GPU nodes")
			} else {
				pmNetReports = stored
				fmt.Fprintf(c.output, "  Loaded topology for %d node(s) from stored report\n", len(stored))
			}
		} else if !rdma.TopologyCoversAllNodes(pmNetReports, gpuNodes) {
			// rdma-node ran this session but topology collection failed for some nodes
			fmt.Fprintf(c.output, "  Warning: in-session topology incomplete (rdma-node failed for some nodes), skipping pingmesh\n")
			pmNetReports = nil
			c.pingmeshReport = rdma.SkipPingMeshReport("Skipped: topology incomplete for all GPU nodes")
		}
		if len(pmNetReports) > 0 {
			if err := c.runPingMesh(ctx, gpuNodes, pmNetReports); err != nil {
				fmt.Fprintf(c.output, "  Warning: pingmesh error: %v\n", err)
			}
		}
	}

	// Step 8: Run multi-node bandwidth jobs (using topology from RDMA node reports)
	var jobResults []jobrunner.JobResult
	needBandwidth := c.opts.CheckMode == CheckModeNetwork || c.opts.CheckMode == CheckModeRDMA || c.opts.CheckMode == CheckModeRDMABandwidth || c.opts.CheckMode == CheckModeAll
	shouldRunBandwidth := needBandwidth && len(c.jobs) > 0 && len(gpuNodes) >= 2
	if shouldRunBandwidth {
		// If net checks didn't run this session, load topology from stored report
		if len(netReports) == 0 {
			stored, topoErr := c.loadTopologyFromReport(ctx, gpuNodes)
			if topoErr != nil {
				fmt.Fprintf(c.output, "  Warning: %v\n", topoErr)
				fmt.Fprintln(c.output, "  Hint: run 'kubectl rhaii-validate rdma-node' first to generate topology")
			} else {
				netReports = stored
				fmt.Fprintf(c.output, "  Loaded topology for %d node(s) from stored report\n", len(stored))
			}
		}

		fmt.Fprintln(c.output, "[Step 9] Running multi-node tests...")
		jr, err := c.runBandwidthJobs(ctx, gpuNodes, netReports)
		if err != nil {
			fmt.Fprintf(c.output, "  Warning: bandwidth test error: %v\n", err)
		}
		jobResults = jr
	}

	// Merge GPU + RDMA node reports for the combined report
	allReports := checks.MergeNodeReports(gpuReports, netReports)

	// Store report in ConfigMap (persists after cleanup)
	if err := c.storeReport(ctx, allReports, jobResults); err != nil {
		fmt.Fprintf(c.output, "  Warning: failed to store report: %v\n", err)
	}

	// Print report
	var hasFailures bool
	if c.opts.OutputFormat == "json" {
		hasFailures = c.printJSONReport(allReports, jobResults)
	} else {
		hasFailures = c.printReport(allReports, jobResults)
	}

	// Debug help is printed by the deferred cleanup. On the success path,
	// show the job logs hint before the defer runs cleanup.
	if c.opts.Debug {
		c.printDebugHelp(ctx)
	} else {
		fmt.Fprintln(c.output, "Cleaning up...")
	}

	totalReports := len(gpuReports) + len(netReports)
	hasPingmesh := c.pingmeshReport != nil
	if totalReports == 0 && !hasPingmesh && len(gpuNodes) > 0 {
		if c.opts.Debug {
			return fmt.Errorf("failed to collect reports — pods kept alive for debugging")
		}
		return fmt.Errorf("failed to collect any reports from %d GPU node(s)", len(gpuNodes))
	}
	expectedReports := 0
	if needGpuChecks {
		expectedReports += len(gpuNodes)
	}
	if needNetChecks {
		expectedReports += len(gpuNodes)
	}
	actualReports := len(gpuReports) + len(netReports)
	if actualReports > 0 && actualReports < expectedReports {
		return fmt.Errorf("partial results: collected %d/%d node reports (some nodes may lack free resources)",
			actualReports, expectedReports)
	}
	if hasFailures {
		return fmt.Errorf("validation failed: one or more checks reported FAIL")
	}

	return nil
}
