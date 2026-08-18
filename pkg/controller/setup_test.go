package controller

import (
	"context"
	"testing"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/config"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestFilterNodes(t *testing.T) {
	tests := []struct {
		name       string
		optsNodes  []string
		discovered []string
		want       []string
	}{
		{
			name:       "no restriction returns all discovered nodes",
			optsNodes:  nil,
			discovered: []string{"node-a", "node-b"},
			want:       []string{"node-a", "node-b"},
		},
		{
			name:       "restricts to allowed nodes only",
			optsNodes:  []string{"node-b"},
			discovered: []string{"node-a", "node-b", "node-c"},
			want:       []string{"node-b"},
		},
		{
			name:       "allowed node not discovered yields empty",
			optsNodes:  []string{"node-z"},
			discovered: []string{"node-a"},
			want:       nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := newTestController(nil)
			c.opts.Nodes = tt.optsNodes

			got := c.filterNodes(tt.discovered)
			if len(got) != len(tt.want) {
				t.Fatalf("filterNodes() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("filterNodes()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestEnsureNamespace(t *testing.T) {
	c, _ := newTestController(nil)
	c.opts.Namespace = "rhaii-validation"

	if err := c.ensureNamespace(context.Background()); err != nil {
		t.Fatalf("ensureNamespace() error = %v", err)
	}

	ns, err := c.client.CoreV1().Namespaces().Get(context.Background(), "rhaii-validation", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected namespace to be created: %v", err)
	}
	if ns.Name != "rhaii-validation" {
		t.Errorf("Namespace name = %q, want %q", ns.Name, "rhaii-validation")
	}

	// Idempotent: calling again when the namespace already exists must not error.
	if err := c.ensureNamespace(context.Background()); err != nil {
		t.Fatalf("ensureNamespace() second call error = %v", err)
	}
}

func gpuNode(name string, labels map[string]string, gpuResource corev1.ResourceName, count int64) *corev1.Node {
	return &corev1.Node{ //nolint:staticcheck
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				gpuResource: *resource.NewQuantity(count, resource.DecimalSI),
			},
		},
	}
}

func TestDiscoverGPUNodes_LabelBased(t *testing.T) {
	client := fake.NewSimpleClientset( //nolint:staticcheck
		gpuNode("gpu-node-1", map[string]string{"nvidia.com/gpu.present": "true"}, "nvidia.com/gpu", 8),
		gpuNode("gpu-node-2", map[string]string{"nvidia.com/gpu.present": "true"}, "nvidia.com/gpu", 8),
		gpuNode("cpu-node-1", nil, "", 0),
	)
	c, _ := newTestController(client)

	nodes, err := c.discoverGPUNodes(context.Background())
	if err != nil {
		t.Fatalf("discoverGPUNodes() error = %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("expected 2 GPU nodes, got %d: %v", len(nodes), nodes)
	}
	if c.gpuVendor != config.GPUVendorNVIDIA {
		t.Errorf("gpuVendor = %q, want %q", c.gpuVendor, config.GPUVendorNVIDIA)
	}
	if c.gpuResource != "nvidia.com/gpu" {
		t.Errorf("gpuResource = %q, want nvidia.com/gpu", c.gpuResource)
	}
	if c.gpuCounts["gpu-node-1"] != 8 {
		t.Errorf("gpuCounts[gpu-node-1] = %d, want 8", c.gpuCounts["gpu-node-1"])
	}
}

func TestDiscoverGPUNodes_LabelBased_SkipsZeroAllocatable(t *testing.T) {
	client := fake.NewSimpleClientset( //nolint:staticcheck
		gpuNode("gpu-node-1", map[string]string{"nvidia.com/gpu.present": "true"}, "nvidia.com/gpu", 0),
	)
	c, _ := newTestController(client)

	nodes, err := c.discoverGPUNodes(context.Background())
	if err != nil {
		t.Fatalf("discoverGPUNodes() error = %v", err)
	}
	if len(nodes) != 0 {
		t.Errorf("expected 0 GPU nodes (0 allocatable should be skipped), got %v", nodes)
	}
}

func TestDiscoverGPUNodes_FallbackAllocatable(t *testing.T) {
	// No GPU node-selector labels present; fallback scans allocatable resources.
	client := fake.NewSimpleClientset( //nolint:staticcheck
		gpuNode("amd-node-1", nil, "amd.com/gpu", 4),
	)
	c, _ := newTestController(client)

	nodes, err := c.discoverGPUNodes(context.Background())
	if err != nil {
		t.Fatalf("discoverGPUNodes() error = %v", err)
	}
	if len(nodes) != 1 || nodes[0] != "amd-node-1" {
		t.Fatalf("expected [amd-node-1], got %v", nodes)
	}
	if c.gpuVendor != config.GPUVendorAMD {
		t.Errorf("gpuVendor = %q, want %q", c.gpuVendor, config.GPUVendorAMD)
	}
}

func TestDiscoverGPUNodes_NoGPUs(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Node{ //nolint:staticcheck
		ObjectMeta: metav1.ObjectMeta{Name: "cpu-only"},
	})
	c, _ := newTestController(client)

	nodes, err := c.discoverGPUNodes(context.Background())
	if err != nil {
		t.Fatalf("discoverGPUNodes() error = %v", err)
	}
	if len(nodes) != 0 {
		t.Errorf("expected no GPU nodes, got %v", nodes)
	}
}

func TestDiscoverGPUNodes_RespectsNodesFilter(t *testing.T) {
	client := fake.NewSimpleClientset( //nolint:staticcheck
		gpuNode("gpu-node-1", map[string]string{"nvidia.com/gpu.present": "true"}, "nvidia.com/gpu", 8),
		gpuNode("gpu-node-2", map[string]string{"nvidia.com/gpu.present": "true"}, "nvidia.com/gpu", 8),
	)
	c, _ := newTestController(client)
	c.opts.Nodes = []string{"gpu-node-2"}

	nodes, err := c.discoverGPUNodes(context.Background())
	if err != nil {
		t.Fatalf("discoverGPUNodes() error = %v", err)
	}
	if len(nodes) != 1 || nodes[0] != "gpu-node-2" {
		t.Fatalf("expected [gpu-node-2], got %v", nodes)
	}
}
