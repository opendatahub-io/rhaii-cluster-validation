package controller

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/opendatahub-io/rhaii-cluster-validation/deploy"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks/crd"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks/operator"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks/rdma"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/config"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/jobrunner"

	"gopkg.in/yaml.v3"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	k8syaml "sigs.k8s.io/yaml"
)

const (
	checkJobLabelKey       = "app"
	gpuCheckJobLabelValue  = "rhaii-validate-gpu-check"
	netCheckJobLabelValue  = "rhaii-validate-net-check"
	bwProbeLabelValue      = "rhaii-validate-bw-probe"
	configMapName = "rhaii-validate-config"

	// BW probe per-test time budget components (seconds).
	// Each GPU-NIC pair runs: server startup + ib_write_bw test + teardown overhead.
	bwProbeServerStartupSecs = 2
	bwProbePerTestSecs       = 30 // matches DefaultPerTestTimeoutSecs
	bwProbeOverheadSecs      = 2
	bwProbePerPairBudgetSecs = bwProbeServerStartupSecs + bwProbePerTestSecs + bwProbeOverheadSecs // ~34s
	bwProbeMinTimeoutSecs    = 900                                                                // 15-minute floor
	reportCMName           = "rhaii-validate-report"
	pingmeshFailuresCMName = "rhaii-validate-pingmesh-failures"
	defaultTimeout         = 5 * time.Minute
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
	client         kubernetes.Interface
	opts           Options
	cfg            config.PlatformConfig
	output         io.Writer
	platform       config.Platform
	gpuVendor      config.GPUVendor    // auto-detected from node labels
	gpuNodeLabel   string              // label used to discover GPU nodes (empty = fallback to resources)
	gpuNodes       []string            // discovered GPU node names
	gpuCounts      map[string]int64    // GPU count per node (from allocatable)
	gpuResource    corev1.ResourceName // e.g. "nvidia.com/gpu" or "amd.com/gpu"
	jobs           []jobrunner.Job
	clusterResults []checks.Result      // Tier 1 (API) check results (CRDs, etc.)
	pingmeshReport        *rdma.PingMeshReport // populated by runPingMesh
	reportStored          bool                 // true after storeReport succeeds
	bwProbeMaxMatrixSize  int                  // largest GPU×NIC matrix across deployed BW probe jobs
}

// AddJob registers a multi-node job to run when --bandwidth is enabled.
func (c *Controller) AddJob(j jobrunner.Job) {
	c.jobs = append(c.jobs, j)
}

// RunCRDChecks checks for required CRDs via the Kubernetes API (Tier 1).
func (c *Controller) RunCRDChecks(ctx context.Context) []checks.Result {
	checker := crd.NewChecker(c.client, nil, c.cfg.CRDs.MinAPIVersions, c.cfg.CRDs.MinReleaseVersions)
	return checker.Run(ctx)
}

// RunOperatorChecks checks that required operators have healthy pods (Tier 1).
func (c *Controller) RunOperatorChecks(ctx context.Context) []checks.Result {
	checker := operator.NewChecker(c.client, nil, c.cfg.Operators.Namespaces)
	return checker.Run(ctx)
}

// RunDeps runs Tier 1 dependency checks (CRDs + operator health) and prints the report.
// This is a lightweight path that doesn't create any cluster resources.
func (c *Controller) RunDeps(ctx context.Context) error {
	// Use stderr for progress so JSON mode stays machine-parseable on stdout
	log := c.output
	if c.opts.OutputFormat == "json" {
		log = io.Discard
	}

	fmt.Fprintln(log, "=== RHAII Dependency Checks ===")
	fmt.Fprintln(log)

	// Detect platform and load config so CRD min versions are available
	fmt.Fprintln(log, "Detecting platform...")
	c.platform = config.DetectPlatform(ctx, c.client)
	cfg, err := config.Load(c.platform, c.opts.ConfigFile)
	if err != nil {
		fmt.Fprintf(log, "  Warning: failed to load config override: %v, using platform defaults\n", err)
		cfg, _ = config.GetConfig(c.platform)
	}
	c.cfg = cfg
	fmt.Fprintf(log, "  Platform: %s\n", c.platform)

	fmt.Fprintln(log, "[CRD Checks] Checking required CRDs...")
	c.clusterResults = c.RunCRDChecks(ctx)
	for _, r := range c.clusterResults {
		fmt.Fprintf(log, "  [%s] %s: %s\n", r.Status, r.Name, r.Message)
	}
	fmt.Fprintln(log)

	fmt.Fprintln(log, "[Operator Checks] Checking operator health...")
	operatorResults := c.RunOperatorChecks(ctx)
	c.clusterResults = append(c.clusterResults, operatorResults...)
	for _, r := range operatorResults {
		fmt.Fprintf(log, "  [%s] %s: %s\n", r.Status, r.Name, r.Message)
	}
	fmt.Fprintln(log)

	var hasFailures bool
	if c.opts.OutputFormat == "json" {
		hasFailures = c.printJSONReport(nil, nil)
	} else {
		hasFailures = c.printReport(nil, nil)
	}

	if hasFailures {
		return fmt.Errorf("dependency check failed: one or more checks reported FAIL")
	}
	return nil
}

// jsonReport is the report structure used for both ConfigMap storage and JSON output.
type jsonReport struct {
	Platform      string                `json:"platform"`
	Timestamp     string                `json:"timestamp,omitempty"`
	ClusterChecks []checks.Result       `json:"cluster_checks,omitempty"`
	Nodes         []checks.NodeReport   `json:"nodes"`
	JobResults    []jobrunner.JobResult `json:"job_results,omitempty"`
	Pingmesh      *rdma.PingMeshReport  `json:"pingmesh,omitempty"`
	Summary       map[string]int        `json:"summary"`
	Status        string                `json:"status"`
}

// countStatuses tallies pass/warn/fail/skip across all result sources.
func countStatuses(clusterResults []checks.Result, reports []checks.NodeReport, jobResults []jobrunner.JobResult, pingmesh *rdma.PingMeshReport) (pass, warn, fail, skip int) {
	for _, r := range clusterResults {
		switch r.Status {
		case checks.StatusPass:
			pass++
		case checks.StatusWarn:
			warn++
		case checks.StatusFail:
			fail++
		case checks.StatusSkip:
			skip++
		}
	}
	for _, report := range reports {
		for _, r := range report.Results {
			switch r.Status {
			case checks.StatusPass:
				pass++
			case checks.StatusWarn:
				warn++
			case checks.StatusFail:
				fail++
			case checks.StatusSkip:
				skip++
			}
		}
	}
	if pingmesh != nil {
		for _, s := range pingmesh.Summary {
			switch s.Status {
			case checks.StatusPass:
				pass++
			case checks.StatusWarn:
				warn++
			case checks.StatusFail:
				fail++
			case checks.StatusSkip:
				skip++
			}
		}
	}
	for _, jr := range jobResults {
		switch jr.Status {
		case checks.StatusPass:
			pass++
		case checks.StatusWarn:
			warn++
		case checks.StatusFail:
			fail++
		}
	}
	return
}

// readinessStatus returns the cluster readiness string based on fail/warn counts.
func readinessStatus(fail, warn int) string {
	if fail > 0 {
		return "NOT READY"
	}
	if warn > 0 {
		return "READY (with warnings)"
	}
	return "READY"
}

// storeReport saves the JSON report to a ConfigMap so it persists after cleanup.
func (c *Controller) storeReport(ctx context.Context, reports []checks.NodeReport, jobResults []jobrunner.JobResult) error {
	pass, warn, fail, skip := countStatuses(c.clusterResults, reports, jobResults, c.pingmeshReport)

	r := jsonReport{
		Platform:      string(c.platform),
		Timestamp:     time.Now().UTC().Format(time.RFC3339),
		ClusterChecks: c.clusterResults,
		Nodes:         reports,
		JobResults:    jobResults,
		Pingmesh:      c.pingmeshReport,
		Summary:       map[string]int{"pass": pass, "warn": warn, "fail": fail, "skip": skip},
		Status:        readinessStatus(fail, warn),
	}

	// Merge with existing report: preserve fields this run didn't produce
	// (e.g. rdma-ping doesn't produce Nodes/JobResults, rdma-bandwidth doesn't produce Pingmesh)
	existing, getErr := c.client.CoreV1().ConfigMaps(c.opts.Namespace).Get(ctx, reportCMName, metav1.GetOptions{})
	if getErr == nil {
		if prev, ok := existing.Data["report.json"]; ok {
			var old jsonReport
			if json.Unmarshal([]byte(prev), &old) == nil {
				if len(r.Nodes) == 0 && len(old.Nodes) > 0 {
					r.Nodes = old.Nodes
				}
				if len(r.JobResults) == 0 && len(old.JobResults) > 0 {
					r.JobResults = old.JobResults
				}
				if r.Pingmesh == nil && old.Pingmesh != nil {
					r.Pingmesh = old.Pingmesh
				}
				// Recompute summary from merged data so preserved FAILs/WARNs are reflected
				pass, warn, fail, skip = countStatuses(r.ClusterChecks, r.Nodes, r.JobResults, r.Pingmesh)
				r.Summary = map[string]int{"pass": pass, "warn": warn, "fail": fail, "skip": skip}
				r.Status = readinessStatus(fail, warn)
			}
		}
	}

	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal report: %w", err)
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      reportCMName,
			Namespace: c.opts.Namespace,
			Labels:    map[string]string{"app": "rhaii-validator"},
		},
		Data: map[string]string{
			"report.json": string(data),
		},
	}

	// Update if exists, create if not
	if getErr == nil {
		existing.Data = cm.Data
		_, err = c.client.CoreV1().ConfigMaps(c.opts.Namespace).Update(ctx, existing, metav1.UpdateOptions{})
	} else if apierrors.IsNotFound(getErr) {
		_, err = c.client.CoreV1().ConfigMaps(c.opts.Namespace).Create(ctx, cm, metav1.CreateOptions{})
	} else {
		return getErr
	}

	if err != nil {
		return err
	}

	c.reportStored = true
	fmt.Fprintf(c.output, "  Report stored in ConfigMap %s/%s\n", c.opts.Namespace, reportCMName)
	return nil
}

// printDebugHelp lists actual pod/job names and useful debug commands.
func (c *Controller) printDebugHelp(ctx context.Context) {
	ns := c.opts.Namespace

	fmt.Fprintln(c.output, "")
	fmt.Fprintln(c.output, "=== DEBUG MODE ===")
	fmt.Fprintln(c.output, "Jobs kept alive for debugging.")
	fmt.Fprintln(c.output, "")

	// List all validation jobs (GPU check + net check + BW probe + bandwidth)
	for _, selector := range []string{
		checkJobLabelKey + "=" + gpuCheckJobLabelValue,
		checkJobLabelKey + "=" + netCheckJobLabelValue,
		checkJobLabelKey + "=" + bwProbeLabelValue,
		"app=rhaii-validate-job",
	} {
		jobs, err := c.client.BatchV1().Jobs(ns).List(ctx, metav1.ListOptions{
			LabelSelector: selector,
		})
		if err != nil || len(jobs.Items) == 0 {
			continue
		}
		fmt.Fprintf(c.output, "Jobs (%s):\n", selector)
		for _, j := range jobs.Items {
			fmt.Fprintf(c.output, "  kubectl logs -n %s -l job-name=%s\n", ns, j.Name)
		}
		fmt.Fprintln(c.output)
	}

	// List pods from check jobs (GPU + RDMA node + BW probe)
	allCheckSelector := checkJobLabelKey + " in (" + gpuCheckJobLabelValue + "," + netCheckJobLabelValue + "," + bwProbeLabelValue + ")"
	pods, err := c.client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: allCheckSelector,
	})
	if err == nil && len(pods.Items) > 0 {
		fmt.Fprintln(c.output, "Pods:")
		for _, pod := range pods.Items {
			fmt.Fprintf(c.output, "  %s (node: %s, status: %s)\n", pod.Name, pod.Spec.NodeName, pod.Status.Phase)
		}
		fmt.Fprintln(c.output)
		fmt.Fprintln(c.output, "View logs:")
		for _, pod := range pods.Items {
			fmt.Fprintf(c.output, "  kubectl logs -n %s %s\n", ns, pod.Name)
		}
	}

	fmt.Fprintln(c.output, "")
	fmt.Fprintf(c.output, "Cleanup: kubectl rhaii-validate clean\n")
}

// printDebugCleanupHint prints the exact commands to manually remove cluster-scoped
// RBAC resources that were left behind by --debug mode.
func (c *Controller) printDebugCleanupHint() {
	ns := c.opts.Namespace
	fmt.Fprintln(c.output, "")
	fmt.Fprintln(c.output, "NOTE: --debug mode skips automatic cleanup. Cluster-scoped RBAC resources remain.")
	fmt.Fprintln(c.output, "To remove them manually:")
	fmt.Fprintf(c.output, "  kubectl delete clusterrolebinding rhaii-validator %s\n", c.sccBindingName())
	fmt.Fprintf(c.output, "  kubectl delete clusterrole rhaii-validator\n")
	fmt.Fprintf(c.output, "  kubectl delete serviceaccount -n %s rhaii-validator\n", ns)
	fmt.Fprintln(c.output, "Or run: kubectl rhaii-validate clean")
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
	if err := c.ensureRBAC(ctx); err != nil {
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
		if err := c.ensureOpenShiftSCC(ctx); err != nil {
			fmt.Fprintf(c.output, "  Warning: failed to create SCC binding: %v\n", err)
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
		fmt.Fprintln(c.output, "[Step 6] Deploying per-node GPU check Jobs...")
		if err := c.deployGpuCheckJobs(ctx); err != nil {
			return fmt.Errorf("failed to deploy GPU check jobs: %w", err)
		}

		fmt.Fprintln(c.output, "  Waiting for GPU check Jobs to complete...")
		gpuReports, err = c.waitAndCollectGpuCheckJobs(ctx)
		if err != nil {
			fmt.Fprintf(c.output, "  Warning: GPU check collection error: %v\n", err)
		}

		if !c.opts.Debug {
			c.cleanupGpuCheckJobs(ctx)
		}
	}

	// Step 7: Deploy per-node RDMA node check Jobs (topology + devices + NIC status)
	var netReports []checks.NodeReport
	needNetChecks := c.opts.CheckMode == CheckModeRDMA || c.opts.CheckMode == CheckModeRDMANode || c.opts.CheckMode == CheckModeAll
	if needNetChecks {
		fmt.Fprintln(c.output, "[Step 7] Deploying per-node RDMA node check Jobs...")
		if err := c.deployNetCheckJobs(ctx); err != nil {
			return fmt.Errorf("failed to deploy RDMA node check jobs: %w", err)
		}

		fmt.Fprintln(c.output, "  Waiting for RDMA node check Jobs to complete...")
		netReports, err = c.waitAndCollectNetCheckJobs(ctx)
		if err != nil {
			fmt.Fprintf(c.output, "  Warning: RDMA node check collection error: %v\n", err)
		}

		// Flat topology detected — net-check is incomplete without BW probe pairing.
		// --debug keeps whichever pods are "final": net-check if flat=false, BW probe if flat=true.
		if needsBandwidthProbe(netReports) {
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
						netReports = c.applyBandwidthPairing(netReports, bwResults)
					}
					if !c.opts.Debug {
						c.cleanupLoopbackBWProbeJobs(ctx)
					}
				}
			}
			// Mark flat nodes that still have NUMA-affinity pairing as WARN.
			// This covers BW probe failures (NVIDIA) and unsupported vendors (AMD).
			netReports = warnUnpairedFlatNodes(netReports)
		} else if !c.opts.Debug {
			c.cleanupNetCheckJobs(ctx)
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
				c.pingmeshReport = skipPingMeshReport("Skipped: topology incomplete for all GPU nodes")
			} else {
				pmNetReports = stored
				fmt.Fprintf(c.output, "  Loaded topology for %d node(s) from stored report\n", len(stored))
			}
		} else if !topologyCoversAllNodes(pmNetReports, gpuNodes) {
			// rdma-node ran this session but topology collection failed for some nodes
			fmt.Fprintf(c.output, "  Warning: in-session topology incomplete (rdma-node failed for some nodes), skipping pingmesh\n")
			pmNetReports = nil
			c.pingmeshReport = skipPingMeshReport("Skipped: topology incomplete for all GPU nodes")
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
	allReports := mergeNodeReports(gpuReports, netReports)

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

func (c *Controller) detectAndCreateConfig(ctx context.Context) error {
	// Detect platform from cluster nodes
	c.platform = config.DetectPlatform(ctx, c.client)
	fmt.Fprintf(c.output, "  Detected platform: %s\n", c.platform)

	// Load embedded defaults (+ optional override from --config file)
	cfg, err := config.Load(c.platform, c.opts.ConfigFile)
	if err != nil {
		return fmt.Errorf("failed to load platform config: %w", err)
	}

	// Serialize config to YAML
	cfgYAML, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("failed to serialize platform config: %w", err)
	}

	// Create ConfigMap with the platform config
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configMapName,
			Namespace: c.opts.Namespace,
			Labels:    map[string]string{"app": "rhaii-validator"},
		},
		Data: map[string]string{
			"platform.yaml": string(cfgYAML),
		},
	}

	// Check if ConfigMap already exists (user may have pre-created or customized it)
	existing, err := c.client.CoreV1().ConfigMaps(c.opts.Namespace).Get(ctx, configMapName, metav1.GetOptions{})
	if err == nil {
		// ConfigMap exists — use it as the sole source of truth (not merged with defaults,
		// because yaml.v3 Unmarshal merges maps instead of replacing them)
		existingYAML, ok := existing.Data["platform.yaml"]
		if !ok {
			return fmt.Errorf("existing ConfigMap %s/%s is missing platform.yaml key — delete it and re-run, or add the key manually",
				c.opts.Namespace, configMapName)
		}
		var cmCfg config.PlatformConfig
		if yamlErr := yaml.Unmarshal([]byte(existingYAML), &cmCfg); yamlErr != nil {
			return fmt.Errorf("failed to parse existing ConfigMap %s/%s platform.yaml: %w",
				c.opts.Namespace, configMapName, yamlErr)
		}
		cfg = cmCfg
		if err := cfg.Validate(); err != nil {
			return fmt.Errorf("existing ConfigMap has invalid config: %w", err)
		}
		c.cfg = cfg
		fmt.Fprintf(c.output, "  ConfigMap %s/%s already exists, using existing config (platform: %s)\n",
			c.opts.Namespace, configMapName, cfg.Platform)
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	// ConfigMap doesn't exist — create it with detected defaults
	_, err = c.client.CoreV1().ConfigMaps(c.opts.Namespace).Create(ctx, cm, metav1.CreateOptions{})
	if err != nil {
		return err
	}

	c.cfg = cfg
	fmt.Fprintf(c.output, "  Created ConfigMap %s/%s (platform: %s)\n", c.opts.Namespace, configMapName, c.platform)
	fmt.Fprintf(c.output, "  To customize: kubectl edit configmap %s -n %s\n", configMapName, c.opts.Namespace)
	return nil
}

// gpuNodeSelectors maps vendor to the node label used to discover GPU nodes.
var gpuNodeSelectors = []struct {
	vendor   config.GPUVendor
	selector string
}{
	{config.GPUVendorNVIDIA, "nvidia.com/gpu.present=true"},
	{config.GPUVendorAMD, "amd.com/gpu.present=true"},
}

func (c *Controller) discoverGPUNodes(ctx context.Context) ([]string, error) {
	c.gpuCounts = make(map[string]int64)

	// Try label-based discovery first
	for _, gs := range gpuNodeSelectors {
		nodes, err := c.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{
			LabelSelector: gs.selector,
		})
		if err != nil {
			continue
		}
		if len(nodes.Items) > 0 {
			c.gpuVendor = gs.vendor
			c.gpuNodeLabel = gs.selector
			c.gpuResource = gpuResourceForVendor(gs.vendor)
			var names []string
			for _, node := range nodes.Items {
				count := gpuCountFromNode(node)
				if count == 0 {
					fmt.Fprintf(c.output, "  Warning: node %s has GPU label but 0 allocatable GPUs, skipping\n", node.Name)
					continue
				}
				names = append(names, node.Name)
				c.gpuCounts[node.Name] = count
			}
			fmt.Fprintf(c.output, "  GPU vendor: %s (auto-detected from node labels)\n", gs.vendor)
			return c.filterNodes(names), nil
		}
	}

	// Fallback: scan all nodes for GPU resources in allocatable
	allNodes, err := c.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}
	var names []string
	for _, node := range allNodes.Items {
		for _, resName := range gpuResourceNames {
			if qty, ok := node.Status.Allocatable[resName]; ok && qty.Value() > 0 {
				names = append(names, node.Name)
				c.gpuCounts[node.Name] = qty.Value()
				if c.gpuVendor == "" {
					if strings.Contains(string(resName), "nvidia") {
						c.gpuVendor = config.GPUVendorNVIDIA
					} else if strings.Contains(string(resName), "amd") {
						c.gpuVendor = config.GPUVendorAMD
					}
					c.gpuResource = resName
				}
				break
			}
		}
	}
	if len(names) > 0 {
		fmt.Fprintf(c.output, "  GPU vendor: %s (auto-detected from node resources)\n", c.gpuVendor)
	}
	return c.filterNodes(names), nil
}

// gpuResourceForVendor returns the GPU resource name for a vendor.
func gpuResourceForVendor(vendor config.GPUVendor) corev1.ResourceName {
	switch vendor {
	case config.GPUVendorAMD:
		return "amd.com/gpu"
	default:
		return "nvidia.com/gpu"
	}
}

// gpuCountFromNode returns the total GPU count from node allocatable.
func gpuCountFromNode(node corev1.Node) int64 {
	for _, resName := range gpuResourceNames {
		if qty, ok := node.Status.Allocatable[resName]; ok && qty.Value() > 0 {
			return qty.Value()
		}
	}
	return 0
}

// filterNodes restricts the discovered node list to only those specified
// in opts.Nodes. If opts.Nodes is empty, all nodes are returned.
func (c *Controller) filterNodes(discovered []string) []string {
	if len(c.opts.Nodes) == 0 {
		return discovered
	}
	allowed := make(map[string]bool, len(c.opts.Nodes))
	for _, n := range c.opts.Nodes {
		allowed[n] = true
	}
	var filtered []string
	for _, n := range discovered {
		if allowed[n] {
			filtered = append(filtered, n)
		}
	}
	return filtered
}

// gpuResourceNames are the known extended resource names for GPUs across vendors.
var gpuResourceNames = []corev1.ResourceName{
	"nvidia.com/gpu",
	"amd.com/gpu",
}

func (c *Controller) ensureNamespace(ctx context.Context) error {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: c.opts.Namespace,
		},
	}
	_, err := c.client.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func (c *Controller) ensureRBAC(ctx context.Context) error {
	// Split multi-document YAML and apply each resource
	docs := splitYAMLDocuments(deploy.RBACYAML)

	for _, doc := range docs {
		if len(doc) == 0 {
			continue
		}

		// Peek at the kind to decide how to unmarshal
		var meta struct {
			Kind string `json:"kind"`
		}
		if err := k8syaml.Unmarshal(doc, &meta); err != nil {
			continue
		}

		switch meta.Kind {
		case "Namespace":
			// Skip — handled by ensureNamespace with the user's --namespace flag
			continue

		case "ServiceAccount":
			var sa corev1.ServiceAccount
			if err := k8syaml.Unmarshal(doc, &sa); err != nil {
				return fmt.Errorf("failed to parse ServiceAccount: %w", err)
			}
			sa.Namespace = c.opts.Namespace
			_, err := c.client.CoreV1().ServiceAccounts(c.opts.Namespace).Create(ctx, &sa, metav1.CreateOptions{})
			if err != nil && !apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("failed to create ServiceAccount: %w", err)
			}
			if err := c.ensurePullSecret(ctx, sa.Name); err != nil {
				return err
			}

		case "ClusterRole":
			var cr rbacv1.ClusterRole
			if err := k8syaml.Unmarshal(doc, &cr); err != nil {
				return fmt.Errorf("failed to parse ClusterRole: %w", err)
			}
			_, err := c.client.RbacV1().ClusterRoles().Create(ctx, &cr, metav1.CreateOptions{})
			if err != nil && !apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("failed to create ClusterRole: %w", err)
			}

		case "ClusterRoleBinding":
			var crb rbacv1.ClusterRoleBinding
			if err := k8syaml.Unmarshal(doc, &crb); err != nil {
				return fmt.Errorf("failed to parse ClusterRoleBinding: %w", err)
			}
			// Update the subject namespace to match --namespace
			for i := range crb.Subjects {
				if crb.Subjects[i].Kind == "ServiceAccount" {
					crb.Subjects[i].Namespace = c.opts.Namespace
				}
			}
			_, err := c.client.RbacV1().ClusterRoleBindings().Create(ctx, &crb, metav1.CreateOptions{})
			if err != nil && !apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("failed to create ClusterRoleBinding: %w", err)
			}

		default:
			fmt.Fprintf(c.output, "  Warning: skipping unknown RBAC resource kind %q\n", meta.Kind)
		}
	}

	return nil
}

// ensurePullSecret ensures the --pull-secret (if specified) is attached to the
// ServiceAccount, and preserves any existing imagePullSecrets already on the SA.
func (c *Controller) ensurePullSecret(ctx context.Context, saName string) error {
	sa, err := c.client.CoreV1().ServiceAccounts(c.opts.Namespace).Get(ctx, saName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get ServiceAccount %s: %w", saName, err)
	}

	secretName := c.opts.PullSecret
	if secretName == "" {
		return nil
	}

	secret, err := c.client.CoreV1().Secrets(c.opts.Namespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("pull secret %q not found in namespace %s — create it first:\n  kubectl create secret docker-registry %s -n %s --from-file=.dockerconfigjson=<path>",
				secretName, c.opts.Namespace, secretName, c.opts.Namespace)
		}
		return fmt.Errorf("failed to get pull secret %q: %w", secretName, err)
	}
	if secret.Type != corev1.SecretTypeDockerConfigJson {
		return fmt.Errorf("secret %q has type %q, expected %q", secretName, secret.Type, corev1.SecretTypeDockerConfigJson)
	}

	for _, ref := range sa.ImagePullSecrets {
		if ref.Name == secretName {
			return nil
		}
	}

	sa.ImagePullSecrets = append(sa.ImagePullSecrets, corev1.LocalObjectReference{Name: secretName})
	_, err = c.client.CoreV1().ServiceAccounts(c.opts.Namespace).Update(ctx, sa, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update ServiceAccount %s with pull secret: %w", saName, err)
	}
	return nil
}

// splitYAMLDocuments splits a multi-document YAML byte slice on "---" separators.
func splitYAMLDocuments(data []byte) [][]byte {
	var docs [][]byte
	for _, part := range strings.Split(string(data), "\n---") {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			docs = append(docs, []byte(trimmed))
		}
	}
	return docs
}

// deployGpuCheckJobs creates one Job per GPU node. Each Job requests all GPUs on
// the node so nvidia-smi (injected by the NVIDIA container runtime) can see
// every GPU for driver and ECC checks.
func (c *Controller) deployGpuCheckJobs(ctx context.Context) error {
	var jobTemplate batchv1.Job
	if err := k8syaml.Unmarshal(deploy.NodeCheckJobYAML, &jobTemplate); err != nil {
		return fmt.Errorf("failed to parse embedded node-check-job.yaml: %w", err)
	}

	for _, nodeName := range c.gpuNodes {
		job := jobTemplate.DeepCopy()

		// Unique name per node
		jobName := fmt.Sprintf("rhaii-validate-check-%s", sanitizeNodeName(nodeName))
		if len(jobName) > 63 {
			jobName = jobName[:63]
		}
		job.Name = jobName
		job.Namespace = c.opts.Namespace

		// Override labels to GPU-check specific value
		job.Labels[checkJobLabelKey] = gpuCheckJobLabelValue
		job.Spec.Template.Labels[checkJobLabelKey] = gpuCheckJobLabelValue

		container := &job.Spec.Template.Spec.Containers[0]
		container.Image = c.opts.Image

		// Pin to specific node
		job.Spec.Template.Spec.NodeSelector = map[string]string{
			"kubernetes.io/hostname": nodeName,
		}

		// Request all GPUs so nvidia-smi sees every GPU
		gpuCount := c.gpuCounts[nodeName]
		if gpuCount > 0 && c.gpuResource != "" {
			gpuQty := resource.MustParse(fmt.Sprintf("%d", gpuCount))
			if container.Resources.Requests == nil {
				container.Resources.Requests = make(corev1.ResourceList)
			}
			if container.Resources.Limits == nil {
				container.Resources.Limits = make(corev1.ResourceList)
			}
			container.Resources.Requests[c.gpuResource] = gpuQty
			container.Resources.Limits[c.gpuResource] = gpuQty
		}

		// Apply agent cpu/memory resources from platform config
		for k, v := range c.cfg.Agent.Requests {
			qty, err := resource.ParseQuantity(v)
			if err != nil {
				return fmt.Errorf("invalid agent resource %q for %s: %w", v, k, err)
			}
			if container.Resources.Requests == nil {
				container.Resources.Requests = make(corev1.ResourceList)
			}
			container.Resources.Requests[corev1.ResourceName(k)] = qty
		}
		for k, v := range c.cfg.Agent.Limits {
			qty, err := resource.ParseQuantity(v)
			if err != nil {
				return fmt.Errorf("invalid agent limit %q for %s: %w", v, k, err)
			}
			if container.Resources.Limits == nil {
				container.Resources.Limits = make(corev1.ResourceList)
			}
			container.Resources.Limits[corev1.ResourceName(k)] = qty
		}

		container.Env = append(container.Env,
			corev1.EnvVar{Name: "GPU_VENDOR", Value: string(c.gpuVendor)},
			corev1.EnvVar{Name: "CHECK_MODE", Value: CheckModeGPU},
		)

		_, err := c.client.BatchV1().Jobs(c.opts.Namespace).Create(ctx, job, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("failed to create GPU check job for node %s: %w", nodeName, err)
		}
		fmt.Fprintf(c.output, "  Created GPU check job %s (node: %s, GPUs: %d)\n", jobName, nodeName, gpuCount)
	}
	return nil
}

// deployNetCheckJobs creates one Job per GPU node for RDMA node checks
// (topology discovery, RDMA device checks, NIC status). Each Job requests
// GPU resources (for nvidia-smi in topology) plus RDMA resources from the
// platform-specific jobs config.
func (c *Controller) deployNetCheckJobs(ctx context.Context) error {
	var jobTemplate batchv1.Job
	if err := k8syaml.Unmarshal(deploy.NodeCheckJobYAML, &jobTemplate); err != nil {
		return fmt.Errorf("failed to parse embedded node-check-job.yaml: %w", err)
	}

	for _, nodeName := range c.gpuNodes {
		job := jobTemplate.DeepCopy()

		jobName := fmt.Sprintf("rhaii-validate-net-%s", sanitizeNodeName(nodeName))
		if len(jobName) > 63 {
			jobName = jobName[:63]
		}
		job.Name = jobName
		job.Namespace = c.opts.Namespace

		// Override labels to net-check specific value
		job.Labels[checkJobLabelKey] = netCheckJobLabelValue
		job.Spec.Template.Labels[checkJobLabelKey] = netCheckJobLabelValue

		container := &job.Spec.Template.Spec.Containers[0]
		container.Image = c.opts.Image

		// Pin to specific node
		job.Spec.Template.Spec.NodeSelector = map[string]string{
			"kubernetes.io/hostname": nodeName,
		}

		if container.Resources.Requests == nil {
			container.Resources.Requests = make(corev1.ResourceList)
		}
		if container.Resources.Limits == nil {
			container.Resources.Limits = make(corev1.ResourceList)
		}

		// Request all GPUs (needed for nvidia-smi in topology discovery)
		gpuCount := c.gpuCounts[nodeName]
		if gpuCount > 0 && c.gpuResource != "" {
			gpuQty := resource.MustParse(fmt.Sprintf("%d", gpuCount))
			container.Resources.Requests[c.gpuResource] = gpuQty
			container.Resources.Limits[c.gpuResource] = gpuQty
		}

		// Apply RDMA + cpu/memory resources from platform jobs config
		for k, v := range c.cfg.Jobs.Requests {
			qty, err := resource.ParseQuantity(v)
			if err != nil {
				return fmt.Errorf("invalid jobs resource %q for %s: %w", v, k, err)
			}
			container.Resources.Requests[corev1.ResourceName(k)] = qty
		}
		for k, v := range c.cfg.Jobs.Limits {
			qty, err := resource.ParseQuantity(v)
			if err != nil {
				return fmt.Errorf("invalid jobs limit %q for %s: %w", v, k, err)
			}
			container.Resources.Limits[corev1.ResourceName(k)] = qty
		}

		// Apply annotations from platform jobs config
		if len(c.cfg.Jobs.Annotations) > 0 {
			if job.Spec.Template.Annotations == nil {
				job.Spec.Template.Annotations = make(map[string]string)
			}
			for k, v := range c.cfg.Jobs.Annotations {
				job.Spec.Template.Annotations[k] = v
			}
		}

		container.Env = append(container.Env,
			corev1.EnvVar{Name: "GPU_VENDOR", Value: string(c.gpuVendor)},
			corev1.EnvVar{Name: "CHECK_MODE", Value: CheckModeRDMANode},
			corev1.EnvVar{Name: "RDMA_TYPE", Value: c.cfg.Jobs.RDMAType},
		)

		_, err := c.client.BatchV1().Jobs(c.opts.Namespace).Create(ctx, job, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("failed to create RDMA node check job for node %s: %w", nodeName, err)
		}
		fmt.Fprintf(c.output, "  Created RDMA node check job %s (node: %s, GPUs: %d)\n", jobName, nodeName, gpuCount)
	}
	return nil
}

// deployLoopbackBWProbeJobs creates one Job per flat-topology node that runs
// intra-host loopback ib_write_bw tests for every GPU-NIC combination.
// All Jobs are created before returning so they run in parallel across nodes.
func (c *Controller) deployLoopbackBWProbeJobs(ctx context.Context, netReports []checks.NodeReport) error {
	topoMap := buildTopologyMap(netReports)

	var maxMatrixSize int
	for _, nodeName := range c.gpuNodes {
		topo := topoMap[nodeName]
		if topo == nil || topo.PairingStrategy != checks.PairingNUMAAffinity {
			continue
		}
		if len(topo.GPUList) == 0 || len(topo.NICList) == 0 {
			fmt.Fprintf(c.output, "  Skipping BW probe for %s: no GPUs or NICs in topology\n", nodeName)
			continue
		}

		gpuIDs := make([]int, len(topo.GPUList))
		for i, g := range topo.GPUList {
			gpuIDs[i] = g.ID
		}
		nicDevs := make([]string, len(topo.NICList))
		for i, n := range topo.NICList {
			nicDevs[i] = n.Dev
		}

		matrixSize := len(gpuIDs) * len(nicDevs)
		if matrixSize > maxMatrixSize {
			maxMatrixSize = matrixSize
		}

		// Use platform config overrides if set, otherwise defaults
		qps := rdma.DefaultLoopbackQPs
		if c.cfg.Jobs.RDMA.QPs > 0 {
			qps = c.cfg.Jobs.RDMA.QPs
		}
		msgSize := rdma.DefaultLoopbackMsgSize
		if c.cfg.Jobs.RDMA.MessageSize > 0 {
			msgSize = c.cfg.Jobs.RDMA.MessageSize
		}
		script, err := rdma.BuildLoopbackScript(gpuIDs, nicDevs,
			rdma.DefaultLoopbackIters, msgSize, rdma.DefaultPerTestTimeoutSecs, qps)
		if err != nil {
			return fmt.Errorf("failed to build BW probe script for node %s: %w", nodeName, err)
		}

		jobName := fmt.Sprintf("rhaii-validate-bwprobe-%s", sanitizeNodeName(nodeName))
		if len(jobName) > 63 {
			h := sha256.Sum256([]byte(jobName))
			suffix := hex.EncodeToString(h[:3])
			prefix := strings.TrimRight(jobName[:56], "-.")
			jobName = prefix + "-" + suffix
		}

		activeDeadlineSecs := int64(matrixSize) * bwProbePerPairBudgetSecs
		if activeDeadlineSecs < bwProbeMinTimeoutSecs {
			activeDeadlineSecs = bwProbeMinTimeoutSecs
		}

		backoffLimit := int32(0)
		privileged := true
		noMount := false
		gracePeriod := int64(5)
		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      jobName,
				Namespace: c.opts.Namespace,
				Labels: map[string]string{
					checkJobLabelKey: bwProbeLabelValue,
				},
			},
			Spec: batchv1.JobSpec{
				BackoffLimit:          &backoffLimit,
				ActiveDeadlineSeconds: &activeDeadlineSecs,
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							checkJobLabelKey: bwProbeLabelValue,
						},
					},
					Spec: corev1.PodSpec{
						RestartPolicy:                 corev1.RestartPolicyNever,
						TerminationGracePeriodSeconds: &gracePeriod,
						ServiceAccountName:            "rhaii-validator",
						AutomountServiceAccountToken:  &noMount,
						Tolerations: []corev1.Toleration{
							{Operator: corev1.TolerationOpExists},
						},
						NodeSelector: map[string]string{
							"kubernetes.io/hostname": nodeName,
						},
						Containers: []corev1.Container{{
							Name:            "bw-probe",
							Image:           c.opts.ToolsImage,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Command:         []string{"/bin/bash", "-c", script},
							SecurityContext: &corev1.SecurityContext{
								Privileged: &privileged,
							},
						}},
					},
				},
			},
		}

		container := &job.Spec.Template.Spec.Containers[0]
		container.Resources.Requests = make(corev1.ResourceList)
		container.Resources.Limits = make(corev1.ResourceList)

		gpuCount := c.gpuCounts[nodeName]
		if gpuCount > 0 && c.gpuResource != "" {
			gpuQty := resource.MustParse(fmt.Sprintf("%d", gpuCount))
			container.Resources.Requests[c.gpuResource] = gpuQty
			container.Resources.Limits[c.gpuResource] = gpuQty
		}

		for k, v := range c.cfg.Jobs.Requests {
			qty, err := resource.ParseQuantity(v)
			if err != nil {
				return fmt.Errorf("invalid jobs resource %q for %s: %w", v, k, err)
			}
			container.Resources.Requests[corev1.ResourceName(k)] = qty
		}
		for k, v := range c.cfg.Jobs.Limits {
			qty, err := resource.ParseQuantity(v)
			if err != nil {
				return fmt.Errorf("invalid jobs limit %q for %s: %w", v, k, err)
			}
			container.Resources.Limits[corev1.ResourceName(k)] = qty
		}

		if len(c.cfg.Jobs.Annotations) > 0 {
			job.Spec.Template.Annotations = make(map[string]string)
			for k, v := range c.cfg.Jobs.Annotations {
				job.Spec.Template.Annotations[k] = v
			}
		}

		_, err = c.client.BatchV1().Jobs(c.opts.Namespace).Create(ctx, job, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("failed to create BW probe job for node %s: %w", nodeName, err)
		}
		fmt.Fprintf(c.output, "  Created BW probe job %s (node: %s, %d GPUs x %d NICs)\n",
			jobName, nodeName, len(gpuIDs), len(nicDevs))
	}

	c.bwProbeMaxMatrixSize = maxMatrixSize
	return nil
}

// waitAndCollectLoopbackBWProbeJobs polls until all BW probe Jobs complete,
// then parses the JSON bandwidth matrix from each pod's logs.
func (c *Controller) waitAndCollectLoopbackBWProbeJobs(ctx context.Context) (map[string]*rdma.LoopbackBWReport, error) {
	selector := checkJobLabelKey + "=" + bwProbeLabelValue

	probeTimeout := time.Duration(c.bwProbeMaxMatrixSize) * bwProbePerPairBudgetSecs * time.Second
	if probeTimeout < bwProbeMinTimeoutSecs*time.Second {
		probeTimeout = bwProbeMinTimeoutSecs * time.Second
	}
	if c.opts.Timeout > probeTimeout {
		probeTimeout = c.opts.Timeout
	}
	fmt.Fprintf(c.output, "  BW probe timeout: %v (matrix size: %d pairs)\n", probeTimeout.Round(time.Second), c.bwProbeMaxMatrixSize)
	fmt.Fprintln(c.output, "  Waiting for loopback BW probe Jobs to complete...")

	timeout := time.After(probeTimeout)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	jobs, err := c.client.BatchV1().Jobs(c.opts.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list BW probe jobs: %w", err)
	}
	expected := len(jobs.Items)
	if expected == 0 {
		return nil, nil
	}

	for {
		select {
		case <-ctx.Done():
			return c.collectLoopbackBWResults(context.Background(), selector)
		case <-timeout:
			fmt.Fprintf(c.output, "  Warning: BW probe timed out after %v, collecting available results\n", probeTimeout)
			return c.collectLoopbackBWResults(ctx, selector)
		case <-ticker.C:
			jobs, err := c.client.BatchV1().Jobs(c.opts.Namespace).List(ctx, metav1.ListOptions{
				LabelSelector: selector,
			})
			if err != nil {
				fmt.Fprintf(c.output, "  Warning: failed to poll BW probe jobs: %v\n", err)
				continue
			}
			completed := 0
			for _, j := range jobs.Items {
				if j.Status.Succeeded > 0 || j.Status.Failed > 0 {
					completed++
				}
			}
			if completed >= expected {
				return c.collectLoopbackBWResults(ctx, selector)
			}
		}
	}
}

// collectLoopbackBWResults gathers and parses JSON output from BW probe pod logs.
func (c *Controller) collectLoopbackBWResults(ctx context.Context, selector string) (map[string]*rdma.LoopbackBWReport, error) {
	jobs, err := c.client.BatchV1().Jobs(c.opts.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list BW probe jobs: %w", err)
	}

	results := make(map[string]*rdma.LoopbackBWReport)
	for _, job := range jobs.Items {
		nodeName := job.Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"]
		if nodeName == "" {
			continue
		}

		pods, err := c.client.CoreV1().Pods(c.opts.Namespace).List(ctx, metav1.ListOptions{
			LabelSelector: "job-name=" + job.Name,
		})
		if err != nil || len(pods.Items) == 0 {
			fmt.Fprintf(c.output, "  Warning: no pod found for BW probe job %s\n", job.Name)
			continue
		}

		stream, err := c.client.CoreV1().Pods(c.opts.Namespace).GetLogs(pods.Items[0].Name, &corev1.PodLogOptions{}).Stream(ctx)
		if err != nil {
			fmt.Fprintf(c.output, "  Warning: failed to get logs from BW probe pod %s: %v\n", pods.Items[0].Name, err)
			continue
		}
		const maxPodLogBytes = 10 << 20 // 10 MiB
		var sb strings.Builder
		io.Copy(&sb, io.LimitReader(stream, maxPodLogBytes))
		stream.Close()

		entries, err := rdma.ParseLoopbackBWOutput(sb.String())
		if err != nil {
			fmt.Fprintf(c.output, "  Warning: failed to parse BW probe output for %s: %v\n", nodeName, err)
			continue
		}

		results[nodeName] = &rdma.LoopbackBWReport{
			Node:    nodeName,
			Results: entries,
		}

		succeeded := 0
		failed := 0
		for _, e := range entries {
			if e.Error != "" {
				failed++
			} else {
				succeeded++
			}
		}
		total := succeeded + failed
		fmt.Fprintf(c.output, "  BW probe %s: %d/%d measurements succeeded, %d failed\n", nodeName, succeeded, total, failed)
	}
	return results, nil
}

// needsBandwidthProbe returns true if any node used NUMA-affinity pairing,
// meaning PCIe-based pairing was not possible (flat topology or missing PCIe paths).
func needsBandwidthProbe(reports []checks.NodeReport) bool {
	for _, r := range reports {
		if topo := checks.ExtractTopology(r); topo != nil && topo.PairingStrategy == checks.PairingNUMAAffinity {
			return true
		}
	}
	return false
}

// warnUnpairedFlatNodes marks the topology check as WARN on flat-topology nodes
// that still use NUMA-affinity pairing after the BW probe phase. This indicates
// the BW probe either failed or was skipped for that node.
func warnUnpairedFlatNodes(reports []checks.NodeReport) []checks.NodeReport {
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

// applyBandwidthPairing replaces NUMA-affinity pairs with bandwidth-optimal
// pairs for nodes that have loopback BW probe results.
func (c *Controller) applyBandwidthPairing(netReports []checks.NodeReport, bwResults map[string]*rdma.LoopbackBWReport) []checks.NodeReport {
	for i, report := range netReports {
		bwReport, ok := bwResults[report.Node]
		if !ok {
			continue
		}

		topo := checks.ExtractTopology(report)
		if topo == nil || topo.PairingStrategy != checks.PairingNUMAAffinity {
			continue
		}

		newPairs := rdma.BandwidthOptimalPairing(bwReport.Results, topo.GPUList, topo.NICList)
		if len(newPairs) == 0 {
			fmt.Fprintf(c.output, "  Warning: BW probe pairing produced no pairs for %s, keeping NUMA-affinity pairing\n", report.Node)
			continue
		}

		topo.Pairs = newPairs
		topo.PairingStrategy = checks.PairingBandwidthProbe
		topo.GPUNICPCIeMapping = rdma.BuildGPUNICPCIeMapping(newPairs)

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

		fmt.Fprintf(c.output, "  Updated %s pairing via bandwidth probe:\n", report.Node)
		for _, p := range newPairs {
			fmt.Fprintf(c.output, "    GPU%d ↔ %s (%.1f Gbps)\n", p.GPU.ID, p.NIC.Dev, p.IntrahostBWGbps)
		}
	}
	return netReports
}

// cleanupLoopbackBWProbeJobs deletes all BW probe jobs and waits for removal.
func (c *Controller) cleanupLoopbackBWProbeJobs(ctx context.Context) bool {
	return c.deleteJobsBySelector(ctx, checkJobLabelKey+"="+bwProbeLabelValue)
}

// waitForPodsGone polls until no pods match the label selector, up to timeout.
func (c *Controller) waitForPodsGone(selector string, timeout time.Duration) {
	deadline := time.After(timeout)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-deadline:
			return
		case <-ticker.C:
			pods, err := c.client.CoreV1().Pods(c.opts.Namespace).List(context.Background(), metav1.ListOptions{
				LabelSelector: selector,
			})
			if err != nil || len(pods.Items) == 0 {
				return
			}
		}
	}
}

// sanitizeNodeName converts a node name to a valid Kubernetes name suffix.
func sanitizeNodeName(name string) string {
	name = strings.ToLower(name)
	name = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		return '-'
	}, name)
	return strings.Trim(name, "-")
}

// waitAndCollectGpuCheckJobs polls until all GPU check Jobs have completed,
// then reads the JSON report from each Job's pod logs.
func (c *Controller) waitAndCollectGpuCheckJobs(ctx context.Context) ([]checks.NodeReport, error) {
	selector := checkJobLabelKey + "=" + gpuCheckJobLabelValue
	return c.waitAndCollectJobsBySelector(ctx, selector, "GPU check", "gpu_hardware")
}

// waitAndCollectNetCheckJobs polls until all RDMA node check Jobs have completed,
// then reads the JSON report from each Job's pod logs.
func (c *Controller) waitAndCollectNetCheckJobs(ctx context.Context) ([]checks.NodeReport, error) {
	selector := checkJobLabelKey + "=" + netCheckJobLabelValue
	return c.waitAndCollectJobsBySelector(ctx, selector, "RDMA node check", "networking_rdma")
}

// waitAndCollectJobsBySelector is the generic polling loop for check Jobs.
func (c *Controller) waitAndCollectJobsBySelector(ctx context.Context, selector, jobKind, checkCategory string) ([]checks.NodeReport, error) {
	timeout := time.After(c.opts.Timeout)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	expected := len(c.gpuNodes)

	for {
		select {
		case <-ctx.Done():
			return c.collectAvailableJobs(ctx, selector, checkCategory, ctx.Err())
		case <-timeout:
			return c.collectAvailableJobs(ctx, selector, checkCategory,
				fmt.Errorf("timed out waiting for %s jobs after %v", jobKind, c.opts.Timeout))
		case <-ticker.C:
			jobs, err := c.client.BatchV1().Jobs(c.opts.Namespace).List(ctx, metav1.ListOptions{
				LabelSelector: selector,
			})
			if err != nil {
				continue
			}

			completed := 0
			failed := 0
			for _, j := range jobs.Items {
				if j.Status.Succeeded > 0 {
					completed++
				} else if j.Status.Failed > 0 {
					completed++
					failed++
				}
			}

			fmt.Fprintf(c.output, "  %s jobs completed: %d/%d", jobKind, completed, expected)
			if failed > 0 {
				fmt.Fprintf(c.output, " (%d failed)", failed)
			}
			fmt.Fprintln(c.output)

			if completed >= expected {
				return c.collectFromJobs(ctx, jobs.Items, checkCategory)
			}
		}
	}
}

// collectFromJobs reads the JSON report from each completed Job's pod logs.
func (c *Controller) collectFromJobs(ctx context.Context, jobs []batchv1.Job, checkCategory string) ([]checks.NodeReport, error) {
	var reports []checks.NodeReport

	for _, job := range jobs {
		pods, err := c.client.CoreV1().Pods(c.opts.Namespace).List(ctx, metav1.ListOptions{
			LabelSelector: "job-name=" + job.Name,
		})
		if err != nil || len(pods.Items) == 0 {
			fmt.Fprintf(c.output, "  Warning: no pod found for job %s\n", job.Name)
			continue
		}

		report, err := c.collectFromPod(ctx, pods.Items[0])
		if err != nil {
			fmt.Fprintf(c.output, "  Warning: %v\n", err)
			nodeName := pods.Items[0].Spec.NodeSelector["kubernetes.io/hostname"]
			if nodeName == "" {
				nodeName = job.Name
			}
			errMsg := err.Error()
			if inner := errors.Unwrap(err); inner != nil {
				errMsg = inner.Error()
			}
			checkNames := checkNamesForCategory(checkCategory)
			var results []checks.Result
			for _, name := range checkNames {
				results = append(results, checks.Result{
					Node:     nodeName,
					Category: checkCategory,
					Name:     name,
					Status:   checks.StatusFail,
					Message:  errMsg,
				})
			}
			reports = append(reports, checks.NodeReport{
				Node:    nodeName,
				Results: results,
			})
			continue
		}
		reports = append(reports, *report)
	}

	return reports, nil
}

// collectAvailableJobs gathers results from whatever Jobs completed before the
// timeout or cancellation. Reports which nodes are missing and returns partial
// results alongside the original error so the caller can still produce a report.
func (c *Controller) collectAvailableJobs(ctx context.Context, selector, checkCategory string, origErr error) ([]checks.NodeReport, error) {
	listCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	jobs, err := c.client.BatchV1().Jobs(c.opts.Namespace).List(listCtx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return nil, origErr
	}

	var completedJobs []batchv1.Job
	for _, j := range jobs.Items {
		if j.Status.Succeeded > 0 || j.Status.Failed > 0 {
			completedJobs = append(completedJobs, j)
		}
	}

	reports, _ := c.collectFromJobs(listCtx, completedJobs, checkCategory)

	collected := make(map[string]bool)
	for _, r := range reports {
		collected[r.Node] = true
	}
	var missing []string
	for _, node := range c.gpuNodes {
		if !collected[node] {
			missing = append(missing, node)
		}
	}

	if len(missing) > 0 {
		fmt.Fprintf(c.output, "  Collected %d/%d node(s); missing: %s\n",
			len(reports), len(c.gpuNodes), strings.Join(missing, ", "))

		for _, j := range jobs.Items {
			if j.Status.Succeeded > 0 || j.Status.Failed > 0 {
				continue
			}
			pods, podErr := c.client.CoreV1().Pods(c.opts.Namespace).List(listCtx, metav1.ListOptions{
				LabelSelector: "job-name=" + j.Name,
			})
			if podErr != nil || len(pods.Items) == 0 {
				continue
			}
			for _, cond := range pods.Items[0].Status.Conditions {
				if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse {
					fmt.Fprintf(c.output, "  Job %s not scheduled: %s\n", j.Name, cond.Message)
				}
			}
		}
	}

	return reports, origErr
}

func (c *Controller) collectFromPod(ctx context.Context, pod corev1.Pod) (*checks.NodeReport, error) {
	stream, err := c.client.CoreV1().Pods(c.opts.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{}).Stream(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get logs from %s: %w", pod.Name, err)
	}
	defer stream.Close()

	report, err := parseReport(stream)
	if err != nil {
		return nil, fmt.Errorf("failed to parse report from %s: %w", pod.Name, err)
	}
	return report, nil
}

func parseReport(r io.Reader) (*checks.NodeReport, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)

	// Skip stderr progress lines until we find the opening "{" of the JSON report.
	// The agent writes JSON to stdout and progress to stderr, but container runtimes
	// (CRI-O, containerd) merge both streams in kubectl logs. We rely on the agent
	// NOT writing to stderr after the JSON (see cmd/agent SilenceErrors) so that
	// json.Decoder can parse the object cleanly. Any trailing stderr text after the
	// closing "}" is ignored by json.Decoder.
	var jsonLines []string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "{") {
			jsonLines = append(jsonLines, line)
			for scanner.Scan() {
				jsonLines = append(jsonLines, scanner.Text())
			}
			break
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading logs: %w", err)
	}

	if len(jsonLines) == 0 {
		return nil, fmt.Errorf("no JSON report found in logs")
	}

	// Use json.Decoder to read exactly one JSON object, ignoring trailing stderr lines
	decoder := json.NewDecoder(strings.NewReader(strings.Join(jsonLines, "\n")))
	var report checks.NodeReport
	if err := decoder.Decode(&report); err != nil {
		return nil, fmt.Errorf("failed to parse JSON: %w", err)
	}

	return &report, nil
}

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
	topoMap := buildTopologyMap(reports)
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

// runPingMesh performs pairwise RDMA connectivity testing across all GPU nodes.
func (c *Controller) runPingMesh(ctx context.Context, gpuNodes []string, netReports []checks.NodeReport) error {
	topoMap := buildTopologyMap(netReports)
	if len(topoMap) == 0 {
		return fmt.Errorf("no topology data available for pingmesh")
	}

	// Determine RDMA type: primary from config, fallback from topology link layer
	rdmaType, err := c.resolvePingMeshRDMAType(topoMap)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.output, "  RDMA type for pingmesh: %s\n", rdmaType)

	gidIndex := c.cfg.Jobs.GetPingGIDIndex()
	iterations := c.cfg.Jobs.PingIterations
	timeout := c.cfg.Jobs.PingTimeout

	// Build RDMA PodConfig (same pattern as RDMABandwidthJob)
	rdmaCfg := &jobrunner.PodConfig{
		ResourceRequests: make(map[string]string),
		ResourceLimits:   make(map[string]string),
		Annotations:      make(map[string]string),
	}
	for k, v := range c.cfg.Jobs.Requests {
		rdmaCfg.ResourceRequests[k] = v
	}
	for k, v := range c.cfg.Jobs.Limits {
		rdmaCfg.ResourceLimits[k] = v
	}
	for k, v := range c.cfg.Jobs.Annotations {
		rdmaCfg.Annotations[k] = v
	}

	toolsImage := c.opts.ToolsImage

	// Build job map for all N-choose-2 pairs
	jobMap := make(map[jobrunner.NodePair]jobrunner.Job)
	for i := 0; i < len(gpuNodes); i++ {
		for j := i + 1; j < len(gpuNodes); j++ {
			nodeA, nodeB := gpuNodes[i], gpuNodes[j]
			topoA, okA := topoMap[nodeA]
			topoB, okB := topoMap[nodeB]
			if !okA || !okB {
				fmt.Fprintf(c.output, "  Warning: missing topology for %s or %s, skipping pair\n", nodeA, nodeB)
				continue
			}

			devsA := devicesFromTopology(topoA)
			devsB := devicesFromTopology(topoB)
			if len(devsA) == 0 || len(devsB) == 0 {
				fmt.Fprintf(c.output, "  Warning: no RDMA NICs for %s or %s, skipping pair\n", nodeA, nodeB)
				continue
			}

			if len(devsA) != len(devsB) {
				fmt.Fprintf(c.output, "  Warning: NIC count mismatch: %s has %d, %s has %d\n", nodeA, len(devsA), nodeB, len(devsB))
			}

			// Canonicalize pair to match roundRobinSchedule ordering (lex-smaller = Server)
			serverNode, clientNode := nodeA, nodeB
			serverDevs, clientDevs := devsA, devsB
			if serverNode > clientNode {
				serverNode, clientNode = clientNode, serverNode
				serverDevs, clientDevs = clientDevs, serverDevs
			}
			pair := jobrunner.NodePair{Server: serverNode, Client: clientNode}
			pmJob := rdma.NewPingMeshJob(serverNode, clientNode, serverDevs, clientDevs, rdmaType, gidIndex, iterations, timeout)
			pmJob.SetPodConfig(rdmaCfg)
			pmJob.SetServerImage(toolsImage)
			pmJob.SetClientImage(toolsImage)
			jobMap[pair] = pmJob
		}
	}

	if len(jobMap) == 0 {
		fmt.Fprintln(c.output, "  No valid node pairs for pingmesh")
		return nil
	}
	fmt.Fprintf(c.output, "  Testing %d node pair(s)\n", len(jobMap))

	runner := jobrunner.New(c.client, c.opts.Namespace, toolsImage, c.opts.Timeout, c.output, c.opts.Debug)
	pairResults, err := runner.RunPairwise(ctx, jobMap, 3)
	if err != nil {
		return fmt.Errorf("pingmesh execution failed: %w", err)
	}

	// Classify results into rail/xrail and build report
	report, failures := c.classifyPingMeshResults(pairResults, topoMap)
	c.pingmeshReport = report

	// Manage detailed failures ConfigMap: update on failure, delete on full success
	if len(failures.Failures) > 0 {
		if err := c.storePingMeshFailures(ctx, failures); err != nil {
			fmt.Fprintf(c.output, "  Warning: failed to store pingmesh failures: %v\n", err)
		}
	} else {
		err := c.client.CoreV1().ConfigMaps(c.opts.Namespace).Delete(ctx, pingmeshFailuresCMName, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			fmt.Fprintf(c.output, "  Warning: failed to clean up old pingmesh failures ConfigMap: %v\n", err)
		}
	}

	return nil
}

// resolvePingMeshRDMAType determines the RDMA type from config or topology.
func (c *Controller) resolvePingMeshRDMAType(topoMap map[string]*checks.NodeTopology) (config.RDMAType, error) {
	if rt := config.RDMAType(c.cfg.Jobs.RDMAType); rt == config.RDMATypeIB || rt == config.RDMATypeRoCE {
		return rt, nil
	}

	// Infer from paired NICs' link layers. Slim pairs don't carry LinkLayer,
	// so look up each paired NIC device in the fully-deserialized NICList.
	linkLayers := make(map[checks.LinkLayer]bool)
	for _, topo := range topoMap {
		nicByDev := make(map[string]checks.LinkLayer, len(topo.NICList))
		for _, nic := range topo.NICList {
			nicByDev[nic.Dev] = nic.LinkLayer
		}
		for _, pair := range topo.Pairs {
			ll, ok := nicByDev[pair.NIC.Dev]
			if !ok {
				return "", fmt.Errorf("paired NIC %q not found in NICList for topology inference", pair.NIC.Dev)
			}
			linkLayers[ll] = true
		}
	}

	switch {
	case len(linkLayers) == 0:
		return "", fmt.Errorf("no NIC link layer data in topology")
	case len(linkLayers) > 1:
		return "", fmt.Errorf("mixed link layer types detected in GPU-paired NICs; set jobs.rdma_type explicitly")
	case linkLayers[checks.LinkLayerEthernet]:
		return config.RDMATypeRoCE, nil
	case linkLayers[checks.LinkLayerInfiniBand]:
		return config.RDMATypeIB, nil
	default:
		return "", fmt.Errorf("unknown link layer type in topology")
	}
}

// devicesFromTopology extracts the list of unique NIC device names from topology Pairs.
func devicesFromTopology(topo *checks.NodeTopology) []string {
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

// classifyPingMeshResults processes RunPairwise results into the PingMeshReport
// and PingMeshFailuresReport, classifying each NIC pair as rail or xrail.
func (c *Controller) classifyPingMeshResults(
	pairResults map[jobrunner.NodePair][]jobrunner.JobResult,
	topoMap map[string]*checks.NodeTopology,
) (*rdma.PingMeshReport, *rdma.PingMeshFailuresReport) {
	var (
		railPassed, railTotal   int
		xrailPassed, xrailTotal int
		matrix                  []rdma.PingMeshNodePair
		allFailures             []rdma.PingMeshFailure
		nodePairCount           int
	)

	for pair, attempts := range pairResults {
		nodePairCount++
		serverTopo, okS := topoMap[pair.Server]
		clientTopo, okC := topoMap[pair.Client]
		if !okS || !okC {
			fmt.Fprintf(c.output, "  Warning: missing topology for %s or %s in classification, skipping pair\n", pair.Server, pair.Client)
			continue
		}

		serverRails := buildRailMap(serverTopo)
		clientRails := buildRailMap(clientTopo)

		// Merge results across retry attempts: a NIC pair passes if it succeeded in any attempt
		type nicPairKey struct{ src, dst string }
		bestResult := make(map[nicPairKey]bool)
		lastError := make(map[nicPairKey]string)
		lastAttempt := make(map[nicPairKey]int)

		for attemptIdx, jr := range attempts {
			results, ok := jr.Details.([]rdma.PingMeshPairResult)
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

		var npRail, npXRail, npAll rdma.PingMeshCount

		for k, passed := range bestResult {
			srcRail, srcOk := clientRails[k.src]
			dstRail, dstOk := serverRails[k.dst]

			isRail := srcOk && dstOk && srcRail == dstRail
			cat := rdma.PingMeshCategoryXRail
			if isRail {
				cat = rdma.PingMeshCategoryRail
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
				allFailures = append(allFailures, rdma.PingMeshFailure{
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
		matrix = append(matrix, rdma.PingMeshNodePair{
			NodeA: nodeA,
			NodeB: nodeB,
			Rail:  npRail,
			XRail: npXRail,
			All:   npAll,
		})
	}

	report := &rdma.PingMeshReport{
		Summary: map[string]rdma.PingMeshCheckSummary{
			"rdma_conn_rail": {
				Status:  pingMeshStatus(railPassed, railTotal),
				Passed:  railPassed,
				Total:   railTotal,
				Message: fmt.Sprintf("Rail RDMA connectivity: %d/%d NIC pairs across %d node pairs", railPassed, railTotal, nodePairCount),
			},
			"rdma_conn_xrail": {
				Status:  pingMeshStatus(xrailPassed, xrailTotal),
				Passed:  xrailPassed,
				Total:   xrailTotal,
				Message: fmt.Sprintf("Cross-rail RDMA connectivity: %d/%d NIC pairs across %d node pairs", xrailPassed, xrailTotal, nodePairCount),
			},
		},
		Matrix: matrix,
	}

	return report, &rdma.PingMeshFailuresReport{
		Timestamp: time.Now().UTC(),
		Failures:  allFailures,
	}
}

// buildRailMap maps NIC device names to their rail index (position in topology Pairs).
func buildRailMap(topo *checks.NodeTopology) map[string]int {
	m := make(map[string]int)
	if topo == nil {
		return m
	}
	for i, pair := range topo.Pairs {
		m[pair.NIC.Dev] = i
	}
	return m
}

// pingMeshStatus returns PASS/WARN/FAIL based on passed/total counts.
// checkNamesForCategory returns the individual check names for a given category,
// used when a node's report fails to parse and we need to emit per-check FAIL rows.
func checkNamesForCategory(category string) []string {
	switch category {
	case "networking_rdma":
		return []string{"rdma_devices_detected", "rdma_nic_status", "gpu_nic_topology"}
	case "gpu_hardware":
		return []string{"gpu_driver_version", "gpu_ecc_status"}
	default:
		return []string{"node_report_collection"}
	}
}

// skipPingMeshReport returns a PingMeshReport with SKIP status for both checks.
func skipPingMeshReport(message string) *rdma.PingMeshReport {
	return &rdma.PingMeshReport{
		Summary: map[string]rdma.PingMeshCheckSummary{
			"rdma_conn_rail":  {Status: checks.StatusSkip, Message: message},
			"rdma_conn_xrail": {Status: checks.StatusSkip, Message: message},
		},
	}
}

func pingMeshStatus(passed, total int) checks.Status {
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

// storePingMeshFailures writes the detailed failures to a separate ConfigMap.
func (c *Controller) storePingMeshFailures(ctx context.Context, failures *rdma.PingMeshFailuresReport) error {
	data, err := json.MarshalIndent(failures, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal pingmesh failures: %w", err)
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pingmeshFailuresCMName,
			Namespace: c.opts.Namespace,
			Labels:    map[string]string{"app": "rhaii-validator"},
		},
		Data: map[string]string{
			"failures.json": string(data),
		},
	}

	existing, err := c.client.CoreV1().ConfigMaps(c.opts.Namespace).Get(ctx, pingmeshFailuresCMName, metav1.GetOptions{})
	if err == nil {
		existing.Data = cm.Data
		_, err = c.client.CoreV1().ConfigMaps(c.opts.Namespace).Update(ctx, existing, metav1.UpdateOptions{})
	} else if apierrors.IsNotFound(err) {
		_, err = c.client.CoreV1().ConfigMaps(c.opts.Namespace).Create(ctx, cm, metav1.CreateOptions{})
	}

	if err != nil {
		return err
	}
	fmt.Fprintf(c.output, "  Pingmesh failures stored in ConfigMap %s/%s\n", c.opts.Namespace, pingmeshFailuresCMName)
	return nil
}

// mergeNodeReports combines reports from multiple phases (e.g. GPU checks and
// RDMA node checks) into a single slice. Reports for the same node are merged
// by appending results.
func mergeNodeReports(reportSets ...[]checks.NodeReport) []checks.NodeReport {
	byNode := make(map[string]*checks.NodeReport)
	var order []string

	for _, reports := range reportSets {
		for _, r := range reports {
			existing, ok := byNode[r.Node]
			if !ok {
				copy := r
				byNode[r.Node] = &copy
				order = append(order, r.Node)
			} else {
				existing.Results = append(existing.Results, r.Results...)
			}
		}
	}

	var merged []checks.NodeReport
	for _, name := range order {
		merged = append(merged, *byNode[name])
	}
	return merged
}

// topologyCoversAllNodes returns true if every node in gpuNodes has topology data in reports.
func topologyCoversAllNodes(reports []checks.NodeReport, gpuNodes []string) bool {
	topoMap := buildTopologyMap(reports)
	for _, n := range gpuNodes {
		if _, ok := topoMap[n]; !ok {
			return false
		}
	}
	return true
}

// buildTopologyMap extracts topology from node reports, keyed by node name.
func buildTopologyMap(reports []checks.NodeReport) map[string]*checks.NodeTopology {
	m := make(map[string]*checks.NodeTopology)
	for _, r := range reports {
		if topo := checks.ExtractTopology(r); topo != nil {
			m[r.Node] = topo
		}
	}
	return m
}

// loadTopologyFromReport reads topology-bearing NodeReports from the stored
// report ConfigMap. Only nodes present in gpuNodes are returned, preserving
// original Result status/details so WARN/FAIL aren't overwritten with PASS.
func (c *Controller) loadTopologyFromReport(ctx context.Context, gpuNodes []string) ([]checks.NodeReport, error) {
	cm, err := c.client.CoreV1().ConfigMaps(c.opts.Namespace).Get(ctx, reportCMName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("no stored report found (ConfigMap %s/%s): %w", c.opts.Namespace, reportCMName, err)
	}

	reportJSON, ok := cm.Data["report.json"]
	if !ok || reportJSON == "" {
		return nil, fmt.Errorf("stored report ConfigMap has no report.json data")
	}

	var stored struct {
		Nodes []checks.NodeReport `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(reportJSON), &stored); err != nil {
		return nil, fmt.Errorf("failed to parse stored report: %w", err)
	}

	nodeSet := make(map[string]bool, len(gpuNodes))
	for _, n := range gpuNodes {
		nodeSet[n] = true
	}

	var reports []checks.NodeReport
	for _, r := range stored.Nodes {
		if !nodeSet[r.Node] {
			continue
		}
		if checks.ExtractTopology(r) != nil {
			reports = append(reports, r)
		}
	}
	if len(reports) == 0 {
		return nil, fmt.Errorf("stored report has no topology data for current GPU nodes")
	}

	if len(reports) < len(gpuNodes) {
		var missing []string
		covered := make(map[string]bool, len(reports))
		for _, r := range reports {
			covered[r.Node] = true
		}
		for _, n := range gpuNodes {
			if !covered[n] {
				missing = append(missing, n)
			}
		}
		return nil, fmt.Errorf("stored report has topology for %d/%d GPU nodes (missing: %s)",
			len(reports), len(gpuNodes), strings.Join(missing, ", "))
	}

	return reports, nil
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

// ensureOpenShiftSCC grants the privileged SCC to the service account.
// The check Jobs need privileged access for host sysfs visibility
// (PCI topology, RDMA device discovery via /sys/class/infiniband).
func (c *Controller) sccBindingName() string {
	return "rhaii-validator-scc-" + c.opts.Namespace
}

func (c *Controller) ensureOpenShiftSCC(ctx context.Context) error {
	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: c.sccBindingName(),
		},
		Subjects: []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      "rhaii-validator",
			Namespace: c.opts.Namespace,
		}},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     "system:openshift:scc:privileged",
		},
	}

	_, err := c.client.RbacV1().ClusterRoleBindings().Create(ctx, crb, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	fmt.Fprintln(c.output, "  OpenShift: granted privileged SCC to rhaii-validator")
	return nil
}

// cleanupGpuCheckJobs deletes all GPU check jobs and waits for them to be fully removed.
func (c *Controller) cleanupGpuCheckJobs(ctx context.Context) bool {
	return c.deleteJobsBySelector(ctx, checkJobLabelKey+"="+gpuCheckJobLabelValue)
}

// cleanupNetCheckJobs deletes all RDMA node check jobs and waits for them to be fully removed.
func (c *Controller) cleanupNetCheckJobs(ctx context.Context) bool {
	return c.deleteJobsBySelector(ctx, checkJobLabelKey+"="+netCheckJobLabelValue)
}

// cleanupBandwidthJobs deletes all bandwidth jobs and waits for them to be fully removed.
func (c *Controller) cleanupBandwidthJobs(ctx context.Context) bool {
	return c.deleteJobsBySelector(ctx, "app=rhaii-validate-job")
}

// cleanupPingMeshJobs deletes pingmesh jobs only. The failures ConfigMap is
// managed by runPingMesh: updated on failure, deleted on full success.
func (c *Controller) cleanupPingMeshJobs(ctx context.Context) bool {
	return c.deleteJobsBySelector(ctx, "rhaii-job-type=pingmesh")
}

// deleteJobsBySelector deletes jobs matching a label selector and waits for pod termination.
// Uses context.Background() so cleanup completes even after signal interruption.
// Uses Background propagation (non-blocking Job GC) and polls for pod termination
// which is the real gate for freeing GPU/RDMA device resources.
// Returns true if all pods terminated within the timeout, false if some remain.
func (c *Controller) deleteJobsBySelector(_ context.Context, selector string) bool {
	bgCtx := context.Background()
	propagation := metav1.DeletePropagationBackground
	jobs, err := c.client.BatchV1().Jobs(c.opts.Namespace).List(bgCtx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil || len(jobs.Items) == 0 {
		return true
	}

	for _, j := range jobs.Items {
		if delErr := c.client.BatchV1().Jobs(c.opts.Namespace).Delete(bgCtx, j.Name, metav1.DeleteOptions{
			PropagationPolicy: &propagation,
		}); delErr != nil && !apierrors.IsNotFound(delErr) {
			fmt.Fprintf(c.output, "  Warning: failed to delete job %s: %v\n", j.Name, delErr)
		}
	}
	fmt.Fprintf(c.output, "  Deleting %d leftover job(s) (%s)...\n", len(jobs.Items), selector)

	for i := 0; i < 30; i++ {
		pods, err := c.client.CoreV1().Pods(c.opts.Namespace).List(bgCtx, metav1.ListOptions{
			LabelSelector: selector,
		})
		if err != nil || len(pods.Items) == 0 {
			return true
		}
		time.Sleep(1 * time.Second)
	}
	fmt.Fprintf(c.output, "  Warning: some pods still terminating for %s\n", selector)
	return false
}

// cleanupAll removes check jobs, bandwidth jobs, and RBAC resources.
// ConfigMap is preserved so users can edit and rerun without losing customizations.
// Uses context.Background() (via deleteJobsBySelector) so cleanup completes after ^C.
func (c *Controller) cleanupAll(ctx context.Context) error {
	c.cleanupGpuCheckJobs(ctx)
	c.cleanupNetCheckJobs(ctx)
	c.cleanupLoopbackBWProbeJobs(ctx)
	c.cleanupBandwidthJobs(ctx)
	c.cleanupPingMeshJobs(ctx)

	bgCtx := context.Background()
	for _, del := range []func() error{
		func() error {
			return c.client.CoreV1().ServiceAccounts(c.opts.Namespace).Delete(bgCtx, "rhaii-validator", metav1.DeleteOptions{})
		},
		func() error {
			return c.client.RbacV1().ClusterRoleBindings().Delete(bgCtx, "rhaii-validator", metav1.DeleteOptions{})
		},
		func() error {
			return c.client.RbacV1().ClusterRoleBindings().Delete(bgCtx, c.sccBindingName(), metav1.DeleteOptions{})
		},
		func() error {
			return c.client.RbacV1().ClusterRoles().Delete(bgCtx, "rhaii-validator", metav1.DeleteOptions{})
		},
	} {
		if err := del(); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (c *Controller) printReport(reports []checks.NodeReport, jobResults []jobrunner.JobResult) bool {
	fmt.Fprintln(c.output)
	fmt.Fprintln(c.output, "=== Validation Report ===")
	fmt.Fprintf(c.output, "Platform: %s\n", c.platform)

	// Print topology if available
	hasTopology := false
	for _, report := range reports {
		if topo := checks.ExtractTopology(report); topo != nil && len(topo.Pairs) > 0 {
			if !hasTopology {
				fmt.Fprintln(c.output)
				fmt.Fprintln(c.output, "GPU-NIC Topology:")
				hasTopology = true
			}
			var pairDescs []string
			for _, p := range topo.Pairs {
				pairDescs = append(pairDescs, fmt.Sprintf("GPU%d↔%s(NUMA:%d↔%d)", p.GPU.ID, p.NIC.Dev, p.GPU.NUMA, p.NIC.NUMA))
			}
			fmt.Fprintf(c.output, "  %s: %s\n", report.Node, strings.Join(pairDescs, ", "))
		}
	}

	fmt.Fprintln(c.output)

	fmt.Fprintf(c.output, "%-20s %-30s %-35s %-8s %s\n", "GROUP", "CHECK", "NODE", "STATUS", "MESSAGE")
	fmt.Fprintln(c.output, strings.Repeat("-", 130))

	for _, r := range c.clusterResults {
		fmt.Fprintf(c.output, "%-20s %-30s %-35s %-8s %s\n",
			r.Category, r.Name, "(cluster)", r.Status, r.Message)

		if r.Remediation != "" {
			fmt.Fprintf(c.output, "%-20s %-30s %-35s %-8s Fix: %s\n",
				"", "", "", "", r.Remediation)
		}
	}

	for _, report := range reports {
		for _, r := range report.Results {
			node := r.Node
			if node == "" {
				node = "-"
			}
			fmt.Fprintf(c.output, "%-20s %-30s %-35s %-8s %s\n",
				r.Category, r.Name, node, r.Status, r.Message)

			if r.Remediation != "" {
				fmt.Fprintf(c.output, "%-20s %-30s %-35s %-8s Fix: %s\n",
					"", "", "", "", r.Remediation)
			}
		}
	}

	// Print pingmesh connectivity results (between per-node checks and bandwidth)
	if c.pingmeshReport != nil {
		for _, name := range []string{"rdma_conn_rail", "rdma_conn_xrail"} {
			if s, ok := c.pingmeshReport.Summary[name]; ok {
				fmt.Fprintf(c.output, "%-20s %-30s %-35s %-8s %s\n",
					"networking_rdma", name, "(cluster)", s.Status, s.Message)
			}
		}
	}

	// Print job results (bandwidth tests)
	for _, jr := range jobResults {
		node := jr.Node
		if node == "" {
			node = "-"
		}
		fmt.Fprintf(c.output, "%-20s %-30s %-35s %-8s %s\n",
			"bandwidth", jr.JobName, node, jr.Status, jr.Message)
	}

	pass, warn, fail, skip := countStatuses(c.clusterResults, reports, jobResults, c.pingmeshReport)

	fmt.Fprintln(c.output)
	fmt.Fprintf(c.output, "Summary: %d PASS | %d WARN | %d FAIL | %d SKIP\n", pass, warn, fail, skip)

	if fail > 0 {
		fmt.Fprintln(c.output, "Status:  NOT READY - resolve FAIL items before deploying")
	} else {
		fmt.Fprintf(c.output, "Status:  %s\n", readinessStatus(fail, warn))
	}

	if c.reportStored {
		fmt.Fprintln(c.output)
		fmt.Fprintln(c.output, "Report:")
		fmt.Fprintf(c.output, "  kubectl get cm %s -n %s -o jsonpath='{.data.report\\.json}' | jq .\n", reportCMName, c.opts.Namespace)
	}
	fmt.Fprintln(c.output)

	return fail > 0
}

func (c *Controller) printJSONReport(reports []checks.NodeReport, jobResults []jobrunner.JobResult) bool {
	pass, warn, fail, skip := countStatuses(c.clusterResults, reports, jobResults, c.pingmeshReport)

	r := jsonReport{
		Platform:      string(c.platform),
		ClusterChecks: c.clusterResults,
		Nodes:         reports,
		JobResults:    jobResults,
		Pingmesh:      c.pingmeshReport,
		Summary:       map[string]int{"pass": pass, "warn": warn, "fail": fail, "skip": skip},
		Status:        readinessStatus(fail, warn),
	}

	data, _ := json.MarshalIndent(r, "", "  ")
	fmt.Fprintln(c.output, string(data))

	return fail > 0
}
