package controller

import (
	"bufio"
	"context"
	"encoding/json"
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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"gopkg.in/yaml.v3"
	k8syaml "sigs.k8s.io/yaml"
)

const (
	checkJobLabelKey       = "app"
	gpuCheckJobLabelValue = "rhaii-validate-gpu-check"
	netCheckJobLabelValue = "rhaii-validate-net-check"
	configMapName      = "rhaii-validate-config"
	reportCMName       = "rhaii-validate-report"
	defaultTimeout     = 5 * time.Minute
)

// Options configures the controller behavior.
type Options struct {
	Kubeconfig   string
	Namespace    string
	Image        string
	Timeout      time.Duration
	ConfigFile   string
	Nodes        []string // Restrict to specific nodes (default: all GPU nodes)
	ServerNode   string
	ClientNodes  []string
	Debug        bool   // Skip cleanup so user can exec into pods for debugging
	OutputFormat string // "table" (default) or "json"
	CheckMode    string // "all" (default), "gpu", "networking"
}

// Controller orchestrates check job deployment, result collection, and cleanup.
type Controller struct {
	client       kubernetes.Interface
	opts         Options
	cfg          config.PlatformConfig
	output       io.Writer
	platform     config.Platform
	gpuVendor    config.GPUVendor   // auto-detected from node labels
	gpuNodeLabel string             // label used to discover GPU nodes (empty = fallback to resources)
	gpuNodes     []string           // discovered GPU node names
	gpuCounts    map[string]int64   // GPU count per node (from allocatable)
	gpuResource    corev1.ResourceName // e.g. "nvidia.com/gpu" or "amd.com/gpu"
	jobs           []jobrunner.Job
	clusterResults []checks.Result // Tier 1 (API) check results (CRDs, etc.)
	reportStored   bool            // true after storeReport succeeds
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
	Platform      string               `json:"platform"`
	Timestamp     string               `json:"timestamp,omitempty"`
	ClusterChecks []checks.Result      `json:"cluster_checks,omitempty"`
	Nodes         []checks.NodeReport  `json:"nodes"`
	JobResults    []jobrunner.JobResult `json:"job_results,omitempty"`
	Summary       map[string]int       `json:"summary"`
	Status        string               `json:"status"`
}

// countStatuses tallies pass/warn/fail/skip across all result sources.
func countStatuses(clusterResults []checks.Result, reports []checks.NodeReport, jobResults []jobrunner.JobResult) (pass, warn, fail, skip int) {
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
	pass, warn, fail, skip := countStatuses(c.clusterResults, reports, jobResults)

	r := jsonReport{
		Platform:      string(c.platform),
		Timestamp:     time.Now().UTC().Format(time.RFC3339),
		ClusterChecks: c.clusterResults,
		Nodes:         reports,
		JobResults:    jobResults,
		Summary:       map[string]int{"pass": pass, "warn": warn, "fail": fail, "skip": skip},
		Status:        readinessStatus(fail, warn),
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
	existing, err := c.client.CoreV1().ConfigMaps(c.opts.Namespace).Get(ctx, reportCMName, metav1.GetOptions{})
	if err == nil {
		existing.Data = cm.Data
		_, err = c.client.CoreV1().ConfigMaps(c.opts.Namespace).Update(ctx, existing, metav1.UpdateOptions{})
	} else if apierrors.IsNotFound(err) {
		_, err = c.client.CoreV1().ConfigMaps(c.opts.Namespace).Create(ctx, cm, metav1.CreateOptions{})
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

	// List all validation jobs (GPU check + net check + bandwidth)
	for _, selector := range []string{
		checkJobLabelKey + "=" + gpuCheckJobLabelValue,
		checkJobLabelKey + "=" + netCheckJobLabelValue,
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

	// List pods from check jobs (GPU + networking)
	allCheckSelector := checkJobLabelKey + " in (" + gpuCheckJobLabelValue + "," + netCheckJobLabelValue + ")"
	pods, err := c.client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: allCheckSelector,
	})
	if err == nil && len(pods.Items) > 0 {
		fmt.Fprintln(c.output, "Check pods:")
		for _, pod := range pods.Items {
			fmt.Fprintf(c.output, "  %s (node: %s, status: %s)\n", pod.Name, pod.Spec.NodeName, pod.Status.Phase)
		}
		fmt.Fprintln(c.output, "")
		fmt.Fprintln(c.output, "Exec into pod:")
		for _, pod := range pods.Items {
			fmt.Fprintf(c.output, "  kubectl exec -it -n %s %s -- bash\n", ns, pod.Name)
		}
	}

	fmt.Fprintln(c.output, "")
	fmt.Fprintln(c.output, "Debug commands inside check pod:")
	fmt.Fprintln(c.output, "  nvidia-smi")
	fmt.Fprintln(c.output, "  chroot /host ibv_devices")
	fmt.Fprintln(c.output, "  chroot /host ibstat")
	fmt.Fprintln(c.output, "  ls /dev/nvidia*")
	fmt.Fprintln(c.output, "")
	fmt.Fprintf(c.output, "Cleanup: kubectl rhaii-validate clean\n")
}

// Cleanup removes all validation resources from the cluster.
func (c *Controller) Cleanup() error {
	ctx := context.Background()
	fmt.Fprintln(c.output, "Cleaning up all validation resources...")

	propagation := metav1.DeletePropagationBackground
	for _, selector := range []string{
		checkJobLabelKey + "=" + gpuCheckJobLabelValue,
		checkJobLabelKey + "=" + netCheckJobLabelValue,
		"app=rhaii-validate-job",
	} {
		jobs, err := c.client.BatchV1().Jobs(c.opts.Namespace).List(ctx, metav1.ListOptions{
			LabelSelector: selector,
		})
		if err == nil {
			for _, j := range jobs.Items {
				_ = c.client.BatchV1().Jobs(c.opts.Namespace).Delete(ctx, j.Name, metav1.DeleteOptions{
					PropagationPolicy: &propagation,
				})
			}
			if len(jobs.Items) > 0 {
				fmt.Fprintf(c.output, "  Deleted %d job(s) (%s)\n", len(jobs.Items), selector)
			}
		}
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

	// Step 1: Cleanup previous runs (GPU check + net check + bandwidth jobs)
	fmt.Fprintln(c.output, "[Step 1] Cleaning up previous runs...")
	c.cleanupGpuCheckJobs(ctx)
	c.cleanupNetCheckJobs(ctx)
	c.cleanupBandwidthJobs(ctx)

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

	// Step 4: Detect platform and create config ConfigMap
	fmt.Fprintln(c.output, "[Step 4] Detecting platform and creating config...")
	if err := c.detectAndCreateConfig(ctx); err != nil {
		return fmt.Errorf("failed to create platform config: %w", err)
	}

	// OpenShift: grant hostmount-anyuid SCC (needed for /host volume mount)
	if c.platform == config.PlatformOCP {
		if err := c.ensureOpenShiftSCC(ctx); err != nil {
			fmt.Fprintf(c.output, "  Warning: failed to create SCC binding: %v\n", err)
		}
	}

	// Step 5: Tier 1 checks (CRDs + operator health)
	if c.opts.CheckMode == "all" || c.opts.CheckMode == "deps" {
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
	needGpuChecks := c.opts.CheckMode == "gpu" || c.opts.CheckMode == "all"
	if needGpuChecks {
		fmt.Fprintln(c.output, "[Step 6] Deploying per-node GPU check Jobs...")
		if err := c.deployGpuCheckJobs(ctx); err != nil {
			return fmt.Errorf("failed to deploy GPU check jobs: %w", err)
		}

		fmt.Fprintln(c.output, "[Step 6] Waiting for GPU check Jobs to complete...")
		gpuReports, err = c.waitAndCollectGpuCheckJobs(ctx)
		if err != nil {
			fmt.Fprintf(c.output, "  Warning: GPU check collection error: %v\n", err)
		}

		if !c.opts.Debug {
			c.cleanupGpuCheckJobs(ctx)
		}
	}

	// Step 7: Deploy per-node networking check Jobs (topology + RDMA)
	var netReports []checks.NodeReport
	needNetChecks := c.opts.CheckMode == "networking" || c.opts.CheckMode == "net-checks" || c.opts.CheckMode == "all"
	if needNetChecks {
		fmt.Fprintln(c.output, "[Step 7] Deploying per-node networking check Jobs...")
		if err := c.deployNetCheckJobs(ctx); err != nil {
			return fmt.Errorf("failed to deploy networking check jobs: %w", err)
		}

		fmt.Fprintln(c.output, "[Step 7] Waiting for networking check Jobs to complete...")
		netReports, err = c.waitAndCollectNetCheckJobs(ctx)
		if err != nil {
			fmt.Fprintf(c.output, "  Warning: networking check collection error: %v\n", err)
		}

		if !c.opts.Debug {
			c.cleanupNetCheckJobs(ctx)
		}
	}

	// Step 8: Run multi-node bandwidth jobs (using topology from networking reports)
	var jobResults []jobrunner.JobResult
	needBandwidth := c.opts.CheckMode == "networking" || c.opts.CheckMode == "net-bandwidth" || c.opts.CheckMode == "all"
	shouldRunBandwidth := needBandwidth && len(c.jobs) > 0 && len(gpuNodes) >= 2
	if shouldRunBandwidth {
		// If net checks didn't run this session, load topology from stored report
		if len(netReports) == 0 {
			stored, topoErr := c.loadTopologyFromReport(ctx, gpuNodes)
			if topoErr != nil {
				fmt.Fprintf(c.output, "  Warning: %v\n", topoErr)
				fmt.Fprintln(c.output, "  Hint: run 'kubectl rhaii-validate net-checks' first to generate topology")
			} else {
				netReports = stored
				fmt.Fprintf(c.output, "  Loaded topology for %d node(s) from stored report\n", len(stored))
			}
		}

		fmt.Fprintln(c.output, "[Step 8] Running multi-node tests...")
		jr, err := c.runBandwidthJobs(ctx, gpuNodes, netReports)
		if err != nil {
			fmt.Fprintf(c.output, "  Warning: bandwidth test error: %v\n", err)
		}
		jobResults = jr
	}

	// Merge GPU + networking reports for the combined report
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

	// Cleanup or keep for debugging
	if c.opts.Debug {
		c.printDebugHelp(ctx)
	} else {
		fmt.Fprintln(c.output, "Cleaning up...")
		if err := c.cleanupAll(ctx); err != nil {
			fmt.Fprintf(c.output, "  Warning: cleanup failed: %v\n", err)
		}
	}

	totalReports := len(gpuReports) + len(netReports)
	if totalReports == 0 && len(gpuNodes) > 0 {
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
		// ConfigMap exists — merge user's overrides on top of detected defaults
		if existingYAML, ok := existing.Data["platform.yaml"]; ok {
			if yamlErr := yaml.Unmarshal([]byte(existingYAML), &cfg); yamlErr != nil {
				return fmt.Errorf("failed to parse existing ConfigMap %s/%s platform.yaml: %w",
					c.opts.Namespace, configMapName, yamlErr)
			}
		}
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
			corev1.EnvVar{Name: "CHECK_MODE", Value: "gpu"},
		)

		_, err := c.client.BatchV1().Jobs(c.opts.Namespace).Create(ctx, job, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("failed to create GPU check job for node %s: %w", nodeName, err)
		}
		fmt.Fprintf(c.output, "  Created GPU check job %s (node: %s, GPUs: %d)\n", jobName, nodeName, gpuCount)
	}
	return nil
}

// deployNetCheckJobs creates one Job per GPU node for networking checks
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
			corev1.EnvVar{Name: "CHECK_MODE", Value: "networking"},
			corev1.EnvVar{Name: "RDMA_TYPE", Value: c.cfg.Jobs.RDMAType},
		)

		_, err := c.client.BatchV1().Jobs(c.opts.Namespace).Create(ctx, job, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("failed to create networking check job for node %s: %w", nodeName, err)
		}
		fmt.Fprintf(c.output, "  Created networking check job %s (node: %s, GPUs: %d)\n", jobName, nodeName, gpuCount)
	}
	return nil
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
	return c.waitAndCollectJobsBySelector(ctx, selector, "GPU check")
}

// waitAndCollectNetCheckJobs polls until all networking check Jobs have completed,
// then reads the JSON report from each Job's pod logs.
func (c *Controller) waitAndCollectNetCheckJobs(ctx context.Context) ([]checks.NodeReport, error) {
	selector := checkJobLabelKey + "=" + netCheckJobLabelValue
	return c.waitAndCollectJobsBySelector(ctx, selector, "networking check")
}

// waitAndCollectJobsBySelector is the generic polling loop for check Jobs.
func (c *Controller) waitAndCollectJobsBySelector(ctx context.Context, selector, jobKind string) ([]checks.NodeReport, error) {
	timeout := time.After(c.opts.Timeout)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	expected := len(c.gpuNodes)

	for {
		select {
		case <-ctx.Done():
			return c.collectAvailableJobs(ctx, selector, ctx.Err())
		case <-timeout:
			return c.collectAvailableJobs(ctx, selector,
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
				return c.collectFromJobs(ctx, jobs.Items)
			}
		}
	}
}

// collectFromJobs reads the JSON report from each completed Job's pod logs.
func (c *Controller) collectFromJobs(ctx context.Context, jobs []batchv1.Job) ([]checks.NodeReport, error) {
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
			continue
		}
		reports = append(reports, *report)
	}

	return reports, nil
}

// collectAvailableJobs gathers results from whatever Jobs completed before the
// timeout or cancellation. Reports which nodes are missing and returns partial
// results alongside the original error so the caller can still produce a report.
func (c *Controller) collectAvailableJobs(ctx context.Context, selector string, origErr error) ([]checks.NodeReport, error) {
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

	reports, _ := c.collectFromJobs(listCtx, completedJobs)

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

// mergeNodeReports combines reports from multiple phases (e.g. GPU checks and
// networking checks) into a single slice. Reports for the same node are merged
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

	// Check if RDMA resource is configured in jobs.requests or jobs.limits
	rdmaAvailable := false
	for _, resources := range []map[string]string{c.cfg.Jobs.Requests, c.cfg.Jobs.Limits} {
		for k := range resources {
			if strings.Contains(k, "rdma") || strings.Contains(k, "roce") || strings.Contains(k, "hca") {
				rdmaAvailable = true
				break
			}
		}
	}

	var jobs []jobrunner.Job
	for _, job := range c.jobs {
		if job.Name() != "ib-write-bw" {
			// Non-RDMA jobs (iperf3): keep as-is
			jobs = append(jobs, job)
			continue
		}

		if !rdmaAvailable {
			fmt.Fprintf(c.output, "  Skipping RDMA jobs: no RDMA device plugin on nodes\n")
			return jobs, []jobrunner.JobResult{{
				JobName: "ib-write-bw",
				Status:  checks.StatusSkip,
				Message: "RDMA skipped: no RDMA device plugin found on nodes (rdma/* resources not in node allocatable)",
			}}
		}

		// Get original job config
		var origPodCfg *jobrunner.PodConfig
		var origServerImg, origClientImg string
		if orig, ok := job.(*rdma.RDMABandwidthJob); ok {
			origPodCfg = orig.PodCfg
			origServerImg = orig.ServerImage
			origClientImg = orig.ClientImage
		}

		// Check how many RDMA devices are requested
		// If requesting all (>= NIC count), we can specify -d per device
		// If requesting 1, let ib_write_bw auto-detect (can't control which device we get)
		rdmaCount := 0
		for k, v := range c.cfg.Jobs.Requests {
			if strings.Contains(k, "rdma") || strings.Contains(k, "roce") || strings.Contains(k, "hca") {
				fmt.Sscanf(v, "%d", &rdmaCount)
				break
			}
		}
		hasAllDevices := rdmaCount >= len(topo.Pairs)

		// GPUDirect RDMA: request all GPUs so the NVIDIA container runtime
		// injects CUDA libraries and --use_cuda sees correct GPU indices
		gpuResourcesAvailable := hasAllDevices && c.gpuResource != "" && topo.GPUCount > 0 && origPodCfg != nil
		if gpuResourcesAvailable {
			gpuCountStr := fmt.Sprintf("%d", topo.GPUCount)
			origPodCfg.ResourceRequests[string(c.gpuResource)] = gpuCountStr
			origPodCfg.ResourceLimits[string(c.gpuResource)] = gpuCountStr
		}

		// Collect devices and GPU IDs for WEP
		var devices []string
		var gpuIDs []int
		uniqueDevices := make(map[string]bool)

		cfgQPs := c.cfg.Jobs.RDMA.QPs
		cfgMsgSize := c.cfg.Jobs.RDMA.MessageSize

		if hasAllDevices {
			// Requesting all RDMA devices — create one PD job per GPU-NIC pair
			for _, pair := range topo.Pairs {
				rdmaJob := rdma.NewRDMABandwidthJob(c.cfg.Thresholds.RDMABandwidthPD.Pass, c.cfg.Thresholds.RDMABandwidthPD.Warn, nil)
				rdmaJob.PodCfg = origPodCfg
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
					devices = append(devices, pair.NIC.Dev)
					gpuIDs = append(gpuIDs, pair.GPU.ID)
					uniqueDevices[pair.NIC.Dev] = true
				}
			}
		} else {
			// Requesting fewer RDMA devices — single PD job, auto-detect device
			rdmaJob := rdma.NewRDMABandwidthJob(c.cfg.Thresholds.RDMABandwidthPD.Pass, c.cfg.Thresholds.RDMABandwidthPD.Warn, nil)
			rdmaJob.PodCfg = origPodCfg
			rdmaJob.ServerImage = origServerImg
			rdmaJob.ClientImage = origClientImg
			if cfgQPs > 0 {
				rdmaJob.QPs = cfgQPs
			}
			if cfgMsgSize > 0 {
				rdmaJob.MessageSize = cfgMsgSize
			}
			// No Device or UseCUDA set — ib_write_bw auto-detects
			jobs = append(jobs, rdmaJob)
			fmt.Fprintf(c.output, "  RDMA PD job: auto-detect device (requesting %d RDMA resource)\n", rdmaCount)
		}

		// Add WEP job if requesting all devices and multiple NICs available
		if hasAllDevices && len(devices) > 1 {
			wepJob := rdma.NewRDMAWEPJob(c.cfg.Thresholds.RDMABandwidthWEP.Pass, c.cfg.Thresholds.RDMABandwidthWEP.Warn, devices, gpuIDs)
			wepJob.PodCfg = origPodCfg
			wepJob.ServerImage = origServerImg
			wepJob.ClientImage = origClientImg
			if cfgQPs > 0 {
				wepJob.QPs = cfgQPs
			}
			if cfgMsgSize > 0 {
				wepJob.MessageSize = cfgMsgSize
			}
			jobs = append(jobs, wepJob)
			fmt.Fprintf(c.output, "  RDMA WEP job: %d NICs in parallel (%s)\n", len(devices), strings.Join(devices, ", "))
		} else {
			fmt.Fprintf(c.output, "  RDMA WEP skipped: only %d NIC(s), need 2+ for whole-endpoint test\n", len(devices))
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

		// Container images from image config
		if imgConfig, ok := job.(jobrunner.ImageConfigurable); ok {
			var jobImage string

			// Special case: tcp-latency uses validator image (has built-in tcp-lat tool)
			if job.Name() == "tcp-latency" {
				jobImage = c.opts.Image
			} else {
				configKey := jobConfigKey(job.Name())
				if configKey == "" {
					continue
				}
				jobImage = c.cfg.Images.GetJobImage(configKey)
				if jobImage == "" {
					continue
				}
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

// jobConfigKey maps job names to image config keys.
func jobConfigKey(jobName string) string {
	if strings.HasPrefix(jobName, "ib-write-bw") {
		return "rdma"
	}
	switch jobName {
	case "iperf3-tcp":
		return "iperf3"
	case "tcp-latency":
		return "" // Uses validator image (built-in tcp-lat tool)
	case "nccl-allreduce":
		return "nccl"
	default:
		return ""
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
// The check Jobs need privileged access for chroot /host to work with
// topology and RDMA checks (host device access, sysfs, ibv_devices, ibstat).
func (c *Controller) ensureOpenShiftSCC(ctx context.Context) error {
	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: "rhaii-validator-scc",
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
func (c *Controller) cleanupGpuCheckJobs(ctx context.Context) {
	c.deleteJobsBySelector(ctx, checkJobLabelKey+"="+gpuCheckJobLabelValue)
}

// cleanupNetCheckJobs deletes all networking check jobs and waits for them to be fully removed.
func (c *Controller) cleanupNetCheckJobs(ctx context.Context) {
	c.deleteJobsBySelector(ctx, checkJobLabelKey+"="+netCheckJobLabelValue)
}

// cleanupBandwidthJobs deletes all bandwidth jobs and waits for them to be fully removed.
func (c *Controller) cleanupBandwidthJobs(ctx context.Context) {
	c.deleteJobsBySelector(ctx, "app=rhaii-validate-job")
}

// deleteJobsBySelector deletes jobs matching a label selector and waits for removal.
func (c *Controller) deleteJobsBySelector(ctx context.Context, selector string) {
	propagation := metav1.DeletePropagationForeground
	jobs, err := c.client.BatchV1().Jobs(c.opts.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil || len(jobs.Items) == 0 {
		return
	}

	for _, j := range jobs.Items {
		_ = c.client.BatchV1().Jobs(c.opts.Namespace).Delete(ctx, j.Name, metav1.DeleteOptions{
			PropagationPolicy: &propagation,
		})
	}
	fmt.Fprintf(c.output, "  Deleting %d leftover job(s) (%s)...\n", len(jobs.Items), selector)

	for i := 0; i < 30; i++ {
		remaining, err := c.client.BatchV1().Jobs(c.opts.Namespace).List(ctx, metav1.ListOptions{
			LabelSelector: selector,
		})
		if err != nil || len(remaining.Items) == 0 {
			return
		}
		time.Sleep(1 * time.Second)
	}
}

// cleanupAll removes check jobs, bandwidth jobs, and RBAC resources.
// ConfigMap is preserved so users can edit and rerun without losing customizations.
func (c *Controller) cleanupAll(ctx context.Context) error {
	c.cleanupGpuCheckJobs(ctx)
	c.cleanupNetCheckJobs(ctx)
	c.cleanupBandwidthJobs(ctx)

	for _, del := range []func() error{
		func() error {
			return c.client.CoreV1().ServiceAccounts(c.opts.Namespace).Delete(ctx, "rhaii-validator", metav1.DeleteOptions{})
		},
		func() error {
			return c.client.RbacV1().ClusterRoleBindings().Delete(ctx, "rhaii-validator", metav1.DeleteOptions{})
		},
		func() error {
			return c.client.RbacV1().ClusterRoleBindings().Delete(ctx, "rhaii-validator-scc", metav1.DeleteOptions{})
		},
		func() error {
			return c.client.RbacV1().ClusterRoles().Delete(ctx, "rhaii-validator", metav1.DeleteOptions{})
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

	// Print job results (bandwidth tests)
	for _, jr := range jobResults {
		node := jr.Node
		if node == "" {
			node = "-"
		}
		fmt.Fprintf(c.output, "%-20s %-30s %-35s %-8s %s\n",
			"bandwidth", jr.JobName, node, jr.Status, jr.Message)
	}

	pass, warn, fail, skip := countStatuses(c.clusterResults, reports, jobResults)

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
	pass, warn, fail, skip := countStatuses(c.clusterResults, reports, jobResults)

	r := jsonReport{
		Platform:      string(c.platform),
		ClusterChecks: c.clusterResults,
		Nodes:         reports,
		JobResults:    jobResults,
		Summary:       map[string]int{"pass": pass, "warn": warn, "fail": fail, "skip": skip},
		Status:        readinessStatus(fail, warn),
	}

	data, _ := json.MarshalIndent(r, "", "  ")
	fmt.Fprintln(c.output, string(data))

	return fail > 0
}
