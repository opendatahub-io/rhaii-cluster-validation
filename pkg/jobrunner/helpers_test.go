package jobrunner

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestBuildJobSpecBasic(t *testing.T) {
	job, err := BuildJobSpec("iperf3-tcp", "gpu-node-0", "rhaii-validation", "myimage:latest",
		RoleClient, nil, []string{"iperf3", "-c", "10.0.0.1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if job.Name != "iperf3-tcp-client-gpu-node-0" {
		t.Errorf("Name = %q, want %q", job.Name, "iperf3-tcp-client-gpu-node-0")
	}
	if job.Namespace != "rhaii-validation" {
		t.Errorf("Namespace = %q, want %q", job.Namespace, "rhaii-validation")
	}

	// Labels
	wantLabels := map[string]string{
		"app":            "rhaii-validate-job",
		"rhaii-job-type": "iperf3-tcp",
		"rhaii-role":     "client",
	}
	for k, want := range wantLabels {
		if got := job.Labels[k]; got != want {
			t.Errorf("Label %q = %q, want %q", k, got, want)
		}
	}

	// Pod template labels match
	for k, want := range wantLabels {
		if got := job.Spec.Template.Labels[k]; got != want {
			t.Errorf("Template label %q = %q, want %q", k, got, want)
		}
	}

	spec := job.Spec.Template.Spec

	// NodeSelector
	if got := spec.NodeSelector["kubernetes.io/hostname"]; got != "gpu-node-0" {
		t.Errorf("NodeSelector hostname = %q, want %q", got, "gpu-node-0")
	}

	// Tolerations
	if len(spec.Tolerations) != 1 || spec.Tolerations[0].Operator != corev1.TolerationOpExists {
		t.Errorf("expected one toleration with Exists operator, got %+v", spec.Tolerations)
	}

	// BackoffLimit
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Errorf("BackoffLimit should be 0")
	}

	// RestartPolicy
	if spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("RestartPolicy = %q, want Never", spec.RestartPolicy)
	}

	// ServiceAccountName
	if spec.ServiceAccountName != "rhaii-validate-agent" {
		t.Errorf("ServiceAccountName = %q, want %q", spec.ServiceAccountName, "rhaii-validate-agent")
	}

	// Container
	if len(spec.Containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(spec.Containers))
	}
	c := spec.Containers[0]
	if c.Image != "myimage:latest" {
		t.Errorf("Image = %q, want %q", c.Image, "myimage:latest")
	}
	if len(c.Command) != 3 || c.Command[0] != "iperf3" {
		t.Errorf("Command = %v, want [iperf3 -c 10.0.0.1]", c.Command)
	}

	// No resources or securityContext with nil PodConfig
	if len(c.Resources.Requests) != 0 || len(c.Resources.Limits) != 0 {
		t.Errorf("expected no resources with nil PodConfig, got requests=%v limits=%v", c.Resources.Requests, c.Resources.Limits)
	}
	if c.SecurityContext != nil {
		t.Errorf("expected nil SecurityContext with nil PodConfig")
	}
}

func TestBuildJobSpecWithPodConfig(t *testing.T) {
	podCfg := &PodConfig{
		ResourceRequests: map[string]string{"nvidia.com/gpu": "8", "cpu": "4"},
		ResourceLimits:   map[string]string{"nvidia.com/gpu": "8"},
		Privileged:       true,
		Annotations:      map[string]string{"test-key": "test-value"},
	}

	job, err := BuildJobSpec("nccl", "node-1", "ns", "img:v1", RoleServer, podCfg, []string{"nccl-test"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	c := job.Spec.Template.Spec.Containers[0]

	// Resources
	if c.Resources.Requests == nil {
		t.Fatal("expected resource requests")
	}
	gpuReq := c.Resources.Requests["nvidia.com/gpu"]
	if gpuReq.String() != "8" {
		t.Errorf("GPU request = %q, want 8", gpuReq.String())
	}
	cpuReq := c.Resources.Requests["cpu"]
	if cpuReq.String() != "4" {
		t.Errorf("CPU request = %q, want 4", cpuReq.String())
	}

	// Limits
	gpuLim := c.Resources.Limits["nvidia.com/gpu"]
	if gpuLim.String() != "8" {
		t.Errorf("GPU limit = %q, want 8", gpuLim.String())
	}

	// Privileged
	if c.SecurityContext == nil || c.SecurityContext.Privileged == nil || !*c.SecurityContext.Privileged {
		t.Error("expected Privileged=true")
	}

	// Annotations
	if got := job.Spec.Template.Annotations["test-key"]; got != "test-value" {
		t.Errorf("Annotation test-key = %q, want %q", got, "test-value")
	}
}

func TestBuildJobSpecLongName(t *testing.T) {
	longNode := "aks-gpupool-10192514-vmss000000000000000000000000000000"
	job, err := BuildJobSpec("iperf3-tcp", longNode, "ns", "img", RoleServer, nil, []string{"test"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(job.Name) > 63 {
		t.Errorf("Name length %d exceeds 63: %q", len(job.Name), job.Name)
	}
	if job.Name[len(job.Name)-1] == '-' {
		t.Errorf("Name ends with dash: %q", job.Name)
	}
}

func TestBuildJobSpecInvalidResource(t *testing.T) {
	podCfg := &PodConfig{
		ResourceRequests: map[string]string{"cpu": "not-a-quantity"},
	}
	_, err := BuildJobSpec("test", "node", "ns", "img", RoleClient, podCfg, []string{"test"})
	if err == nil {
		t.Fatal("expected error for invalid resource quantity")
	}
}
