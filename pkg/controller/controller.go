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
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/config"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/jobrunner"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"gopkg.in/yaml.v3"
	k8syaml "sigs.k8s.io/yaml"
)

const (
	agentLabelKey   = "app"
	agentLabelValue = "rhaii-validate-agent"
	configMapName   = "rhaii-validate-config"
	defaultTimeout  = 5 * time.Minute
	annotationKey   = "rhaii.opendatahub.io/validation-status"
	annotationDone  = "done"
	annotationError = "error"
)

// Options configures the controller behavior.
type Options struct {
	Kubeconfig  string
	Namespace   string
	Image       string
	Timeout     time.Duration
	ConfigFile  string
	ServerNode  string
	ClientNodes []string
}

// Controller orchestrates agent deployment, result collection, and cleanup.
type Controller struct {
	client   kubernetes.Interface
	opts     Options
	cfg      config.PlatformConfig
	output   io.Writer
	platform config.Platform
	jobs     []jobrunner.Job
}

// AddJob registers a multi-node job to run when --bandwidth is enabled.
func (c *Controller) AddJob(j jobrunner.Job) {
	c.jobs = append(c.jobs, j)
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

	// Step 1: Cleanup previous DaemonSet (preserve existing ConfigMap)
	fmt.Fprintln(c.output, "[Step 1] Cleaning up previous runs...")
	if err := c.cleanupDaemonSet(ctx); err != nil {
		fmt.Fprintf(c.output, "  Warning: cleanup failed: %v\n", err)
	}

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

	// Step 5: Discover GPU nodes
	fmt.Fprintln(c.output, "[Step 5] Discovering GPU nodes...")
	gpuNodes, err := c.discoverGPUNodes(ctx)
	if err != nil {
		return fmt.Errorf("failed to discover GPU nodes: %w", err)
	}
	if len(gpuNodes) == 0 {
		fmt.Fprintln(c.output, "  No GPU nodes found. Nothing to validate.")
		return nil
	}
	fmt.Fprintf(c.output, "  Found %d GPU node(s): %s\n", len(gpuNodes), strings.Join(gpuNodes, ", "))

	// Step 6: Deploy agent DaemonSet
	fmt.Fprintln(c.output, "[Step 6] Deploying agent DaemonSet...")
	if err := c.deployAgent(ctx); err != nil {
		return fmt.Errorf("failed to deploy agent: %w", err)
	}

	// Step 7: Wait for agents and collect results
	fmt.Fprintln(c.output, "[Step 7] Waiting for agents to complete and collecting results...")
	reports, err := c.waitAndCollect(ctx, gpuNodes)
	if err != nil {
		fmt.Fprintf(c.output, "  Warning: collection error: %v\n", err)
	}

	// Step 8: Run multi-node jobs (automatically when 2+ GPU nodes and jobs registered)
	var jobResults []jobrunner.JobResult
	if len(c.jobs) > 0 && len(gpuNodes) >= 2 {
		fmt.Fprintln(c.output, "[Step 8] Running multi-node tests...")
		jr, err := c.runBandwidthJobs(ctx, gpuNodes)
		if err != nil {
			fmt.Fprintf(c.output, "  Warning: bandwidth test error: %v\n", err)
		}
		jobResults = jr
	}

	// Cleanup DaemonSet, ConfigMap, RBAC
	fmt.Fprintln(c.output, "Cleaning up...")
	if err := c.cleanupAll(ctx); err != nil {
		fmt.Fprintf(c.output, "  Warning: cleanup failed: %v\n", err)
	}

	// Print report
	hasFailures := c.printReport(reports, jobResults)

	if len(reports) == 0 && len(gpuNodes) > 0 {
		return fmt.Errorf("failed to collect any reports from %d GPU node(s)", len(gpuNodes))
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
			Labels:    map[string]string{agentLabelKey: agentLabelValue},
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
			// Unmarshal user config on top of detected defaults (only set fields are overridden)
			if yamlErr := yaml.Unmarshal([]byte(existingYAML), &cfg); yamlErr != nil {
				fmt.Fprintf(c.output, "  Warning: failed to parse existing ConfigMap YAML: %v\n", yamlErr)
			}
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

func (c *Controller) discoverGPUNodes(ctx context.Context) ([]string, error) {
	nodes, err := c.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: "nvidia.com/gpu.present=true",
	})
	if err != nil {
		return nil, err
	}

	var names []string
	for _, node := range nodes.Items {
		names = append(names, node.Name)
	}
	return names, nil
}

// gpuResourceNames are the known extended resource names for GPUs across vendors.
var gpuResourceNames = []corev1.ResourceName{
	"nvidia.com/gpu",
	"amd.com/gpu",
}

// detectGPUResources scans GPU nodes and returns the GPU resource name and the
// minimum allocatable count across all nodes.
func (c *Controller) detectGPUResources(ctx context.Context, gpuNodes []string) (string, int64) {
	var detectedResource string
	var minCount int64

	for _, nodeName := range gpuNodes {
		node, err := c.client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			continue
		}
		for _, resName := range gpuResourceNames {
			if qty, ok := node.Status.Allocatable[resName]; ok {
				count := qty.Value()
				if count > 0 {
					detectedResource = string(resName)
					if minCount == 0 || count < minCount {
						minCount = count
					}
					break
				}
			}
		}
	}
	return detectedResource, minCount
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

func (c *Controller) deployAgent(ctx context.Context) error {
	var ds appsv1.DaemonSet
	if err := k8syaml.Unmarshal(deploy.DaemonSetYAML, &ds); err != nil {
		return fmt.Errorf("failed to parse embedded daemonset.yaml: %w", err)
	}

	// Override dynamic fields
	ds.Namespace = c.opts.Namespace
	ds.Spec.Template.Spec.Containers[0].Image = c.opts.Image

	// Apply podConfiguration from platform config (resource requests, annotations, etc.)
	podCfg := c.cfg.PodConfiguration
	if len(podCfg.ResourceRequests) > 0 || len(podCfg.ResourceLimits) > 0 {
		reqs := corev1.ResourceRequirements{}
		if len(podCfg.ResourceRequests) > 0 {
			reqs.Requests = make(corev1.ResourceList)
			for k, v := range podCfg.ResourceRequests {
				qty, err := resource.ParseQuantity(v)
				if err != nil {
					return fmt.Errorf("invalid resource request %q for %s: %w", v, k, err)
				}
				reqs.Requests[corev1.ResourceName(k)] = qty
			}
		}
		if len(podCfg.ResourceLimits) > 0 {
			reqs.Limits = make(corev1.ResourceList)
			for k, v := range podCfg.ResourceLimits {
				qty, err := resource.ParseQuantity(v)
				if err != nil {
					return fmt.Errorf("invalid resource limit %q for %s: %w", v, k, err)
				}
				reqs.Limits[corev1.ResourceName(k)] = qty
			}
		}
		ds.Spec.Template.Spec.Containers[0].Resources = reqs
	}
	if len(podCfg.Annotations) > 0 {
		if ds.Spec.Template.Annotations == nil {
			ds.Spec.Template.Annotations = make(map[string]string)
		}
		for k, v := range podCfg.Annotations {
			ds.Spec.Template.Annotations[k] = v
		}
	}

	_, err := c.client.AppsV1().DaemonSets(c.opts.Namespace).Create(ctx, &ds, metav1.CreateOptions{})
	return err
}

func (c *Controller) waitAndCollect(ctx context.Context, gpuNodes []string) ([]checks.NodeReport, error) {
	timeout := time.After(c.opts.Timeout)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	selector := labels.Set{agentLabelKey: agentLabelValue}.AsSelector().String()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timeout:
			return nil, fmt.Errorf("timed out waiting for agents after %v", c.opts.Timeout)
		case <-ticker.C:
			pods, err := c.client.CoreV1().Pods(c.opts.Namespace).List(ctx, metav1.ListOptions{
				LabelSelector: selector,
			})
			if err != nil {
				continue
			}

			// Check annotation set by the agent when it finishes.
			ready := 0
			for _, pod := range pods.Items {
				status := pod.Annotations[annotationKey]
				if status == annotationDone || status == annotationError {
					ready++
				}
			}

			fmt.Fprintf(c.output, "  Agents ready: %d/%d\n", ready, len(gpuNodes))

			if ready >= len(gpuNodes) {
				return c.collectResults(ctx, pods.Items)
			}
		}
	}
}

func (c *Controller) collectResults(ctx context.Context, pods []corev1.Pod) ([]checks.NodeReport, error) {
	var reports []checks.NodeReport

	for _, pod := range pods {
		report, err := c.collectFromPod(ctx, pod)
		if err != nil {
			fmt.Fprintf(c.output, "  Warning: %v\n", err)
			continue
		}
		reports = append(reports, *report)
	}

	return reports, nil
}

func (c *Controller) collectFromPod(ctx context.Context, pod corev1.Pod) (*checks.NodeReport, error) {
	// Try current logs first (agent sets "done" annotation before exiting),
	// fall back to previous logs if the container has already restarted.
	stream, err := c.client.CoreV1().Pods(c.opts.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{}).Stream(ctx)
	if err != nil {
		stream, err = c.client.CoreV1().Pods(c.opts.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
			Previous: true,
		}).Stream(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to get logs from %s: %w", pod.Name, err)
		}
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

	// Skip stderr lines until we find the start of JSON
	var jsonLines []string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "{") {
			jsonLines = append(jsonLines, line)
			// Collect remaining lines (json.Decoder will stop at the right place)
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

func (c *Controller) runBandwidthJobs(ctx context.Context, gpuNodes []string) ([]jobrunner.JobResult, error) {
	if len(c.jobs) == 0 {
		fmt.Fprintf(c.output, "  No jobs registered, skipping bandwidth tests\n")
		return nil, nil
	}

	serverNode := c.opts.ServerNode
	clientNodes := c.opts.ClientNodes

	// Default: first GPU node is server, rest are clients
	if serverNode == "" && len(gpuNodes) > 0 {
		serverNode = gpuNodes[0]
	}
	if len(clientNodes) == 0 && len(gpuNodes) > 1 {
		clientNodes = gpuNodes[1:]
	}

	if serverNode == "" || len(clientNodes) == 0 {
		return nil, fmt.Errorf("need at least 2 GPU nodes for bandwidth tests (have %d)", len(gpuNodes))
	}

	fmt.Fprintf(c.output, "  Server: %s, Clients: %s\n", serverNode, strings.Join(clientNodes, ", "))

	// Auto-detect GPU resources and set PodConfig on jobs that support it
	gpuResource, gpuCount := c.detectGPUResources(ctx, gpuNodes)
	if gpuResource != "" && gpuCount > 0 {
		fmt.Fprintf(c.output, "  Detected GPU resource: %s (count: %d per node)\n", gpuResource, gpuCount)
		countStr := fmt.Sprintf("%d", gpuCount)
		podCfg := &jobrunner.PodConfig{
			ResourceRequests: map[string]string{gpuResource: countStr},
			ResourceLimits:   map[string]string{gpuResource: countStr},
		}
		for _, job := range c.jobs {
			if configurable, ok := job.(jobrunner.Configurable); ok {
				configurable.SetPodConfig(podCfg)
			}
		}
	}

	// Set thresholds from platform config
	for _, job := range c.jobs {
		if tc, ok := job.(jobrunner.ThresholdConfigurable); ok {
			switch job.Name() {
			case "iperf3-tcp":
				tc.SetThreshold(c.cfg.Thresholds.TCPBandwidth.Pass)
			case "ib-write-bw":
				tc.SetThreshold(c.cfg.Thresholds.RDMABandwidthPD.Pass)
			}
		}
	}

	runner := jobrunner.New(c.client, c.opts.Namespace, c.opts.Image, c.opts.Timeout, c.output)

	// Run all registered jobs
	var allResults []jobrunner.JobResult
	for _, job := range c.jobs {
		fmt.Fprintf(c.output, "  Running job: %s\n", job.Name())
		results, err := runner.RunJob(ctx, job, serverNode, clientNodes)
		if err != nil {
			fmt.Fprintf(c.output, "  Warning: job %s failed: %v\n", job.Name(), err)
			allResults = append(allResults, jobrunner.JobResult{
				JobName: job.Name(),
				Role:    jobrunner.RoleClient,
				Status:  checks.StatusFail,
				Message: fmt.Sprintf("job failed: %v", err),
			})
			continue
		}
		allResults = append(allResults, results...)
	}

	return allResults, nil
}

// cleanupDaemonSet removes only the agent DaemonSet, preserving the ConfigMap.
// Used at the start of a run to clean up leftover DaemonSets from previous runs
// without destroying user-customized ConfigMaps.
func (c *Controller) cleanupDaemonSet(ctx context.Context) error {
	err := c.client.AppsV1().DaemonSets(c.opts.Namespace).Delete(ctx, "rhaii-validate-agent", metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// cleanupAll removes the agent DaemonSet and RBAC resources.
// ConfigMap is preserved so users can edit and rerun without losing customizations.
func (c *Controller) cleanupAll(ctx context.Context) error {
	if err := c.cleanupDaemonSet(ctx); err != nil {
		return err
	}

	for _, del := range []func() error{
		func() error {
			return c.client.CoreV1().ServiceAccounts(c.opts.Namespace).Delete(ctx, "rhaii-validate-agent", metav1.DeleteOptions{})
		},
		func() error {
			return c.client.RbacV1().ClusterRoleBindings().Delete(ctx, "rhaii-validate-agent", metav1.DeleteOptions{})
		},
		func() error {
			return c.client.RbacV1().ClusterRoles().Delete(ctx, "rhaii-validate-agent", metav1.DeleteOptions{})
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
	fmt.Fprintln(c.output)

	pass, warn, fail, skip := 0, 0, 0, 0

	fmt.Fprintf(c.output, "%-20s %-30s %-35s %-8s %s\n", "GROUP", "CHECK", "NODE", "STATUS", "MESSAGE")
	fmt.Fprintln(c.output, strings.Repeat("-", 130))

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

	// Print job results (bandwidth tests)
	for _, jr := range jobResults {
		node := jr.Node
		if node == "" {
			node = "-"
		}
		fmt.Fprintf(c.output, "%-20s %-30s %-35s %-8s %s\n",
			"bandwidth", jr.JobName, node, jr.Status, jr.Message)

		switch jr.Status {
		case checks.StatusPass:
			pass++
		case checks.StatusWarn:
			warn++
		case checks.StatusFail:
			fail++
		}
	}

	fmt.Fprintln(c.output)
	fmt.Fprintf(c.output, "Summary: %d PASS | %d WARN | %d FAIL | %d SKIP\n", pass, warn, fail, skip)

	if fail > 0 {
		fmt.Fprintln(c.output, "Status:  NOT READY - resolve FAIL items before deploying")
	} else if warn > 0 {
		fmt.Fprintln(c.output, "Status:  READY (with warnings)")
	} else {
		fmt.Fprintln(c.output, "Status:  READY")
	}
	fmt.Fprintln(c.output)

	return fail > 0
}
