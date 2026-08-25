package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks/rdma"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/jobrunner"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// deployLoopbackBWProbeJobs creates one Job per flat-topology node that runs
// intra-host loopback ib_write_bw tests for every GPU-NIC combination.
// All Jobs are created before returning so they run in parallel across nodes.
func (c *Controller) deployLoopbackBWProbeJobs(ctx context.Context, netReports []checks.NodeReport) error {
	topoMap := rdma.BuildTopologyMap(netReports)

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
		gpuCount := c.gpuCounts[nodeName]
		jobrunner.SetGPUResource(container, c.gpuResource, gpuCount)

		if err := jobrunner.ApplyResourceConfig(container, c.cfg.Jobs.Requests, c.cfg.Jobs.Limits); err != nil {
			return fmt.Errorf("invalid BW probe resource config for node %s: %w", nodeName, err)
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
