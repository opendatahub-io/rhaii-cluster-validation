package jobrunner

import (
	"testing"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
	batchv1 "k8s.io/api/batch/v1"
)

func TestToResourceRequirementsValid(t *testing.T) {
	pc := &PodConfig{
		ResourceRequests: map[string]string{"nvidia.com/gpu": "8", "cpu": "4", "memory": "16Gi"},
		ResourceLimits:   map[string]string{"nvidia.com/gpu": "8"},
	}

	reqs, err := pc.ToResourceRequirements()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(reqs.Requests) != 3 {
		t.Errorf("expected 3 requests, got %d", len(reqs.Requests))
	}
	if gpu := reqs.Requests["nvidia.com/gpu"]; gpu.String() != "8" {
		t.Errorf("GPU request = %q, want 8", gpu.String())
	}
	if cpu := reqs.Requests["cpu"]; cpu.String() != "4" {
		t.Errorf("CPU request = %q, want 4", cpu.String())
	}
	if mem := reqs.Requests["memory"]; mem.String() != "16Gi" {
		t.Errorf("Memory request = %q, want 16Gi", mem.String())
	}

	if len(reqs.Limits) != 1 {
		t.Errorf("expected 1 limit, got %d", len(reqs.Limits))
	}
	if gpu := reqs.Limits["nvidia.com/gpu"]; gpu.String() != "8" {
		t.Errorf("GPU limit = %q, want 8", gpu.String())
	}
}

func TestToResourceRequirementsEmpty(t *testing.T) {
	pc := &PodConfig{}
	reqs, err := pc.ToResourceRequirements()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(reqs.Requests) != 0 {
		t.Errorf("expected empty requests, got %v", reqs.Requests)
	}
	if len(reqs.Limits) != 0 {
		t.Errorf("expected empty limits, got %v", reqs.Limits)
	}
}

func TestToResourceRequirementsInvalid(t *testing.T) {
	tests := []struct {
		name string
		pc   PodConfig
	}{
		{
			name: "invalid request",
			pc:   PodConfig{ResourceRequests: map[string]string{"cpu": "abc"}},
		},
		{
			name: "invalid limit",
			pc:   PodConfig{ResourceLimits: map[string]string{"memory": "not-valid"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.pc.ToResourceRequirements()
			if err == nil {
				t.Fatal("expected error for invalid resource value")
			}
		})
	}
}

// MockJobWithCustomImages implements both Job and ImageConfigurable
type MockJobWithCustomImages struct {
	serverImg string
	clientImg string
}

func (m *MockJobWithCustomImages) Name() string           { return "mock-custom-img" }
func (m *MockJobWithCustomImages) GetServerImage() string { return m.serverImg }
func (m *MockJobWithCustomImages) GetClientImage() string { return m.clientImg }
func (m *MockJobWithCustomImages) ServerSpec(node, namespace, image string) (*batchv1.Job, error) {
	return BuildJobSpec(m.Name(), node, namespace, image, RoleServer, nil, []string{"echo", image})
}
func (m *MockJobWithCustomImages) ClientSpec(node, namespace, image, serverIP string) (*batchv1.Job, error) {
	return BuildJobSpec(m.Name(), node, namespace, image, RoleClient, nil, []string{"echo", image})
}
func (m *MockJobWithCustomImages) ParseResult(logs string) (*JobResult, error) {
	return &JobResult{Status: checks.StatusPass}, nil
}

// MockJobNoCustomImages implements only Job interface (not ImageConfigurable)
type MockJobNoCustomImages struct{}

func (m *MockJobNoCustomImages) Name() string { return "mock-default-img" }
func (m *MockJobNoCustomImages) ServerSpec(node, namespace, image string) (*batchv1.Job, error) {
	return BuildJobSpec(m.Name(), node, namespace, image, RoleServer, nil, []string{"echo", image})
}
func (m *MockJobNoCustomImages) ClientSpec(node, namespace, image, serverIP string) (*batchv1.Job, error) {
	return BuildJobSpec(m.Name(), node, namespace, image, RoleClient, nil, []string{"echo", image})
}
func (m *MockJobNoCustomImages) ParseResult(logs string) (*JobResult, error) {
	return &JobResult{Status: checks.StatusPass}, nil
}

func TestImageConfigurable(t *testing.T) {
	tests := []struct {
		name                string
		job                 Job
		wantServerImage     string
		wantClientImage     string
		implementsInterface bool
	}{
		{
			name: "job with custom images",
			job: &MockJobWithCustomImages{
				serverImg: "custom-server:v1",
				clientImg: "custom-client:v2",
			},
			wantServerImage:     "custom-server:v1",
			wantClientImage:     "custom-client:v2",
			implementsInterface: true,
		},
		{
			name: "job with same custom image",
			job: &MockJobWithCustomImages{
				serverImg: "same-img:latest",
				clientImg: "same-img:latest",
			},
			wantServerImage:     "same-img:latest",
			wantClientImage:     "same-img:latest",
			implementsInterface: true,
		},
		{
			name: "job with empty server image (fallback)",
			job: &MockJobWithCustomImages{
				serverImg: "",
				clientImg: "client-only:v1",
			},
			wantServerImage:     "",
			wantClientImage:     "client-only:v1",
			implementsInterface: true,
		},
		{
			name:                "job without ImageConfigurable",
			job:                 &MockJobNoCustomImages{},
			wantServerImage:     "",
			wantClientImage:     "",
			implementsInterface: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Test interface implementation
			imgConfig, ok := tt.job.(ImageConfigurable)
			if ok != tt.implementsInterface {
				t.Errorf("ImageConfigurable check = %v, want %v", ok, tt.implementsInterface)
			}

			if tt.implementsInterface {
				if got := imgConfig.GetServerImage(); got != tt.wantServerImage {
					t.Errorf("GetServerImage() = %q, want %q", got, tt.wantServerImage)
				}
				if got := imgConfig.GetClientImage(); got != tt.wantClientImage {
					t.Errorf("GetClientImage() = %q, want %q", got, tt.wantClientImage)
				}
			}
		})
	}
}

func TestJobSpecWithCustomImage(t *testing.T) {
	job := &MockJobWithCustomImages{
		serverImg: "my-server:v1",
		clientImg: "my-client:v2",
	}

	// Test server spec receives custom image
	serverJob, err := job.ServerSpec("node1", "test-ns", "my-server:v1")
	if err != nil {
		t.Fatalf("ServerSpec failed: %v", err)
	}
	if serverJob.Spec.Template.Spec.Containers[0].Image != "my-server:v1" {
		t.Errorf("Server container image = %q, want %q",
			serverJob.Spec.Template.Spec.Containers[0].Image, "my-server:v1")
	}

	// Test client spec receives custom image
	clientJob, err := job.ClientSpec("node2", "test-ns", "my-client:v2", "10.0.0.1")
	if err != nil {
		t.Fatalf("ClientSpec failed: %v", err)
	}
	if clientJob.Spec.Template.Spec.Containers[0].Image != "my-client:v2" {
		t.Errorf("Client container image = %q, want %q",
			clientJob.Spec.Template.Spec.Containers[0].Image, "my-client:v2")
	}
}

func TestJobSpecWithDefaultImage(t *testing.T) {
	job := &MockJobNoCustomImages{}

	// Test server spec with default image
	serverJob, err := job.ServerSpec("node1", "test-ns", "default:latest")
	if err != nil {
		t.Fatalf("ServerSpec failed: %v", err)
	}
	if serverJob.Spec.Template.Spec.Containers[0].Image != "default:latest" {
		t.Errorf("Server container image = %q, want %q",
			serverJob.Spec.Template.Spec.Containers[0].Image, "default:latest")
	}

	// Test client spec with default image
	clientJob, err := job.ClientSpec("node2", "test-ns", "default:latest", "10.0.0.1")
	if err != nil {
		t.Fatalf("ClientSpec failed: %v", err)
	}
	if clientJob.Spec.Template.Spec.Containers[0].Image != "default:latest" {
		t.Errorf("Client container image = %q, want %q",
			clientJob.Spec.Template.Spec.Containers[0].Image, "default:latest")
	}
}
