package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/config"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestSanitizeNodeName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "already valid", in: "gpu-node-1", want: "gpu-node-1"},
		{name: "uppercase lowered", in: "GPU-NODE-1", want: "gpu-node-1"},
		{name: "dots and colons replaced", in: "aks-gpupool.vmss:15", want: "aks-gpupool-vmss-15"},
		{name: "leading/trailing dashes trimmed", in: ".node.", want: "node"},
		{name: "fqdn-style name", in: "ip-10-0-1-2.ec2.internal", want: "ip-10-0-1-2-ec2-internal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeNodeName(tt.in); got != tt.want {
				t.Errorf("sanitizeNodeName(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestCheckNamesForCategory(t *testing.T) {
	tests := []struct {
		category string
		want     []string
	}{
		{category: "networking_rdma", want: []string{"rdma_devices_detected", "rdma_nic_status", "gpu_nic_topology"}},
		{category: "gpu_hardware", want: []string{"gpu_driver_version", "gpu_ecc_status"}},
		{category: "unknown_category", want: []string{"node_report_collection"}},
	}
	for _, tt := range tests {
		t.Run(tt.category, func(t *testing.T) {
			got := checkNamesForCategory(tt.category)
			if len(got) != len(tt.want) {
				t.Fatalf("checkNamesForCategory(%q) = %v, want %v", tt.category, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("checkNamesForCategory(%q)[%d] = %q, want %q", tt.category, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestCollectFromJobs_NoPodFound(t *testing.T) {
	c, buf := newTestController(nil)

	job := batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "rhaii-validate-check-node1", Namespace: c.opts.Namespace}}
	reports, err := c.collectFromJobs(context.Background(), []batchv1.Job{job}, "gpu_hardware")
	if err != nil {
		t.Fatalf("collectFromJobs() error = %v", err)
	}
	if len(reports) != 0 {
		t.Errorf("expected no reports when no pod matches the job, got %v", reports)
	}
	if !strings.Contains(buf.String(), "no pod found for job") {
		t.Errorf("expected warning about missing pod, got output: %s", buf.String())
	}
}

func TestCollectFromJobs_UnparsableLogsProduceFailResults(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck
	c, _ := newTestController(client)

	job := batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "rhaii-validate-check-node1", Namespace: c.opts.Namespace}}
	pod := &corev1.Pod{ //nolint:staticcheck
		ObjectMeta: metav1.ObjectMeta{
			Name:      "rhaii-validate-check-node1-abcde",
			Namespace: c.opts.Namespace,
			Labels:    map[string]string{"job-name": job.Name},
		},
		Spec: corev1.PodSpec{
			NodeSelector: map[string]string{"kubernetes.io/hostname": "node1"},
		},
	}
	if _, err := client.CoreV1().Pods(c.opts.Namespace).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to seed pod: %v", err)
	}

	// The fake clientset's GetLogs always returns "fake logs" (not valid JSON),
	// so collectFromPod fails to parse and collectFromJobs falls back to
	// synthesizing FAIL results for every check in the category.
	reports, err := c.collectFromJobs(context.Background(), []batchv1.Job{job}, "gpu_hardware")
	if err != nil {
		t.Fatalf("collectFromJobs() error = %v", err)
	}
	if len(reports) != 1 {
		t.Fatalf("expected 1 synthesized report, got %d", len(reports))
	}
	if reports[0].Node != "node1" {
		t.Errorf("Node = %q, want %q", reports[0].Node, "node1")
	}
	wantChecks := checkNamesForCategory("gpu_hardware")
	if len(reports[0].Results) != len(wantChecks) {
		t.Fatalf("expected %d FAIL results, got %d", len(wantChecks), len(reports[0].Results))
	}
	for _, r := range reports[0].Results {
		if r.Status != "FAIL" {
			t.Errorf("Result %s status = %s, want FAIL", r.Name, r.Status)
		}
	}
}

func TestCollectAvailableJobs_ReportsMissingNodes(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck
	c, buf := newTestController(client)
	c.gpuNodes = []string{"node1", "node2"}

	// No jobs exist at all for the selector, so nothing is collected and both
	// nodes should be reported missing.
	origErr := context.DeadlineExceeded
	reports, err := c.collectAvailableJobs(context.Background(), "app=rhaii-validate-gpu-check", "gpu_hardware", origErr)
	if err != origErr {
		t.Errorf("collectAvailableJobs() error = %v, want %v", err, origErr)
	}
	if len(reports) != 0 {
		t.Errorf("expected no reports, got %v", reports)
	}
	if !strings.Contains(buf.String(), "missing: node1, node2") {
		t.Errorf("expected missing-node warning, got output: %s", buf.String())
	}
}

func TestDeployNodeCheckJobs_EFAInjection(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck
	c, buf := newTestController(client)
	c.platform = config.PlatformEKS
	c.gpuNodes = []string{"p5-node"}
	c.gpuCounts = map[string]int64{"p5-node": 8}
	c.efaCounts = map[string]int64{"p5-node": 32}
	c.gpuResource = "nvidia.com/gpu"
	c.gpuVendor = config.GPUVendorNVIDIA
	c.opts.Image = "validator:test"
	c.cfg.Jobs = config.ResourceConfig{
		Requests: map[string]string{
			"cpu": "500m",
		},
		RDMAType: "ib",
	}

	err := c.deployNodeCheckJobs(context.Background(), nodeCheckJobSpec{
		kind:        "RDMA node check",
		namePrefix:  "rhaii-validate-net-",
		labelValue:  netCheckJobLabelValue,
		resourceCfg: c.cfg.Jobs,
		checkMode:   CheckModeRDMANode,
		setRDMAType: true,
	})
	if err != nil {
		t.Fatalf("deployNodeCheckJobs() error = %v", err)
	}

	jobs, err := client.BatchV1().Jobs(c.opts.Namespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs.Items))
	}
	container := jobs.Items[0].Spec.Template.Spec.Containers[0]
	efaQty := container.Resources.Requests[config.EFAResourceName]
	if efaQty.Value() != 32 {
		t.Errorf("EFA requests = %d, want 32", efaQty.Value())
	}
	var rdmaType string
	for _, env := range container.Env {
		switch env.Name {
		case "RDMA_TYPE":
			rdmaType = env.Value
		}
	}
	if rdmaType != "srd" {
		t.Errorf("RDMA_TYPE = %q, want srd", rdmaType)
	}
	if !strings.Contains(buf.String(), "EFA: 32") {
		t.Errorf("expected EFA log line, got: %s", buf.String())
	}
}

func TestDeployNodeCheckJobs_EFAConfigOverridesAutoDetect(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck
	c, _ := newTestController(client)
	c.platform = config.PlatformEKS
	c.gpuNodes = []string{"p5-node"}
	c.gpuCounts = map[string]int64{"p5-node": 8}
	c.efaCounts = map[string]int64{"p5-node": 32}
	c.gpuResource = "nvidia.com/gpu"
	c.opts.Image = "validator:test"
	c.cfg.Jobs = config.ResourceConfig{
		Requests: map[string]string{
			"cpu":                   "500m",
			"vpc.amazonaws.com/efa": "16",
		},
	}

	if err := c.deployNodeCheckJobs(context.Background(), nodeCheckJobSpec{
		kind:        "RDMA node check",
		namePrefix:  "rhaii-validate-net-",
		labelValue:  netCheckJobLabelValue,
		resourceCfg: c.cfg.Jobs,
		checkMode:   CheckModeRDMANode,
		setRDMAType: true,
	}); err != nil {
		t.Fatalf("deployNodeCheckJobs() error = %v", err)
	}

	jobs, _ := client.BatchV1().Jobs(c.opts.Namespace).List(context.Background(), metav1.ListOptions{})
	container := jobs.Items[0].Spec.Template.Spec.Containers[0]
	efaQty := container.Resources.Requests[config.EFAResourceName]
	if efaQty.Value() != 16 {
		t.Errorf("EFA requests = %d, want 16 (config override)", efaQty.Value())
	}
}

func TestDeployNodeCheckJobs_NoEFAOnNonEKS(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck
	c, _ := newTestController(client)
	c.platform = config.PlatformCoreWeave
	c.gpuNodes = []string{"gpu-node"}
	c.gpuCounts = map[string]int64{"gpu-node": 8}
	c.efaCounts = map[string]int64{"gpu-node": 32}
	c.gpuResource = "nvidia.com/gpu"
	c.opts.Image = "validator:test"
	c.cfg.Jobs = config.ResourceConfig{
		Requests: map[string]string{"cpu": "500m"},
		RDMAType: "ib",
	}

	if err := c.deployNodeCheckJobs(context.Background(), nodeCheckJobSpec{
		kind:        "RDMA node check",
		namePrefix:  "rhaii-validate-net-",
		labelValue:  netCheckJobLabelValue,
		resourceCfg: c.cfg.Jobs,
		checkMode:   CheckModeRDMANode,
		setRDMAType: true,
	}); err != nil {
		t.Fatalf("deployNodeCheckJobs() error = %v", err)
	}

	jobs, _ := client.BatchV1().Jobs(c.opts.Namespace).List(context.Background(), metav1.ListOptions{})
	container := jobs.Items[0].Spec.Template.Spec.Containers[0]
	if qty, ok := container.Resources.Requests[config.EFAResourceName]; ok {
		t.Errorf("non-EKS should not inject EFA, got %v", qty)
	}
}
