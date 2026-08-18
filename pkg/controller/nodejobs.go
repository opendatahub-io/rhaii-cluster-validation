package controller

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/opendatahub-io/rhaii-cluster-validation/deploy"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/config"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/jobrunner"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8syaml "sigs.k8s.io/yaml"
)

// nodeCheckJobSpec parameterizes the per-node check Job builder shared by the
// GPU-check and RDMA-node-check phases: both deploy the same embedded
// node-check-job.yaml template pinned to a single node, differing only in
// name/label, which ResourceConfig supplies cpu/memory, and the env vars
// passed to the agent's "run" subcommand.
type nodeCheckJobSpec struct {
	kind        string // used in log/error messages, e.g. "GPU check"
	namePrefix  string
	labelValue  string
	resourceCfg config.ResourceConfig
	checkMode   string
	setRDMAType bool
}

// deployNodeCheckJobs creates one Job per GPU node from the embedded
// node-check-job.yaml template, pinned to that node and requesting all of its
// GPUs so nvidia-smi (injected by the GPU container runtime) sees every GPU.
func (c *Controller) deployNodeCheckJobs(ctx context.Context, spec nodeCheckJobSpec) error {
	var jobTemplate batchv1.Job
	if err := k8syaml.Unmarshal(deploy.NodeCheckJobYAML, &jobTemplate); err != nil {
		return fmt.Errorf("failed to parse embedded node-check-job.yaml: %w", err)
	}

	for _, nodeName := range c.gpuNodes {
		job := jobTemplate.DeepCopy()

		// Unique name per node
		jobName := fmt.Sprintf("%s%s", spec.namePrefix, sanitizeNodeName(nodeName))
		if len(jobName) > 63 {
			jobName = jobName[:63]
		}
		job.Name = jobName
		job.Namespace = c.opts.Namespace

		job.Labels[checkJobLabelKey] = spec.labelValue
		job.Spec.Template.Labels[checkJobLabelKey] = spec.labelValue

		container := &job.Spec.Template.Spec.Containers[0]
		container.Image = c.opts.Image

		// Pin to specific node
		job.Spec.Template.Spec.NodeSelector = map[string]string{
			"kubernetes.io/hostname": nodeName,
		}

		// Request all GPUs so the agent sees every GPU on the node
		gpuCount := c.gpuCounts[nodeName]
		jobrunner.SetGPUResource(container, c.gpuResource, gpuCount)

		if err := jobrunner.ApplyResourceConfig(container, spec.resourceCfg.Requests, spec.resourceCfg.Limits); err != nil {
			return fmt.Errorf("invalid %s resource config: %w", spec.kind, err)
		}

		if len(spec.resourceCfg.Annotations) > 0 {
			if job.Spec.Template.Annotations == nil {
				job.Spec.Template.Annotations = make(map[string]string)
			}
			for k, v := range spec.resourceCfg.Annotations {
				job.Spec.Template.Annotations[k] = v
			}
		}

		container.Env = append(container.Env,
			corev1.EnvVar{Name: "GPU_VENDOR", Value: string(c.gpuVendor)},
			corev1.EnvVar{Name: "CHECK_MODE", Value: spec.checkMode},
		)
		if spec.setRDMAType {
			container.Env = append(container.Env, corev1.EnvVar{Name: "RDMA_TYPE", Value: spec.resourceCfg.RDMAType})
		}

		_, err := c.client.BatchV1().Jobs(c.opts.Namespace).Create(ctx, job, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("failed to create %s job for node %s: %w", spec.kind, nodeName, err)
		}
		fmt.Fprintf(c.output, "  Created %s job %s (node: %s, GPUs: %d)\n", spec.kind, jobName, nodeName, gpuCount)
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
