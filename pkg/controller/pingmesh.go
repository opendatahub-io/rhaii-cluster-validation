package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks/rdma"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/jobrunner"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// runPingMesh performs pairwise RDMA connectivity testing across all GPU nodes.
func (c *Controller) runPingMesh(ctx context.Context, gpuNodes []string, netReports []checks.NodeReport) error {
	topoMap := rdma.BuildTopologyMap(netReports)
	if len(topoMap) == 0 {
		return fmt.Errorf("no topology data available for pingmesh")
	}

	// Determine RDMA type: primary from config, fallback from topology link layer
	rdmaType, err := rdma.ResolvePingMeshRDMAType(c.cfg.Jobs.RDMAType, topoMap)
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

			devsA := rdma.DevicesFromTopology(topoA)
			devsB := rdma.DevicesFromTopology(topoB)
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
	report, failures := rdma.ClassifyPingMeshResults(pairResults, topoMap, c.output)
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
