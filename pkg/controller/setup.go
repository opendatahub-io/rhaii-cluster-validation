package controller

import (
	"context"
	"fmt"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/config"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

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

func (c *Controller) discoverGPUNodes(ctx context.Context) ([]string, error) {
	c.gpuCounts = make(map[string]int64)

	// Try label-based discovery first
	for _, gs := range config.GPUNodeSelectors {
		nodes, err := c.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{
			LabelSelector: gs.Selector,
		})
		if err != nil {
			continue
		}
		if len(nodes.Items) > 0 {
			c.gpuVendor = gs.Vendor
			c.gpuNodeLabel = gs.Selector
			c.gpuResource = config.GPUResourceForVendor(gs.Vendor)
			var names []string
			for _, node := range nodes.Items {
				count := config.GPUCountFromAllocatable(node.Status.Allocatable)
				if count == 0 {
					fmt.Fprintf(c.output, "  Warning: node %s has GPU label but 0 allocatable GPUs, skipping\n", node.Name)
					continue
				}
				names = append(names, node.Name)
				c.gpuCounts[node.Name] = count
			}
			fmt.Fprintf(c.output, "  GPU vendor: %s (auto-detected from node labels)\n", gs.Vendor)
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
		for _, resName := range config.GPUResourceNames {
			if qty, ok := node.Status.Allocatable[resName]; ok && qty.Value() > 0 {
				names = append(names, node.Name)
				c.gpuCounts[node.Name] = qty.Value()
				if c.gpuVendor == "" {
					c.gpuVendor = config.GPUVendorFromResourceName(resName)
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
