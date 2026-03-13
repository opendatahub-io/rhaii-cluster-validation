package jobrunner

import "testing"

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
