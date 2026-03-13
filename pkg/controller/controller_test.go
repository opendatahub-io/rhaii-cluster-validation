package controller

import (
	"strings"
	"testing"
)

func TestParseReport(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantNode string
		wantLen  int
		wantErr  bool
	}{
		{
			name: "stderr lines then JSON",
			input: `[PASS] gpu_hardware/gpu_driver_version: NVIDIA driver: 535.129.03
[PASS] gpu_hardware/gpu_ecc_status: No errors
[FAIL] networking_rdma/rdma_devices_detected: No RDMA devices
{
  "node": "gpu-node-1",
  "timestamp": "2024-01-01T00:00:00Z",
  "results": [
    {
      "category": "gpu_hardware",
      "name": "gpu_driver_version",
      "status": "PASS",
      "message": "OK"
    },
    {
      "category": "networking_rdma",
      "name": "rdma_devices_detected",
      "status": "FAIL",
      "message": "No RDMA devices"
    }
  ]
}`,
			wantNode: "gpu-node-1",
			wantLen:  2,
		},
		{
			name: "JSON only no stderr",
			input: `{
  "node": "node-2",
  "timestamp": "2024-01-01T00:00:00Z",
  "results": [
    {
      "category": "gpu_hardware",
      "name": "gpu_ecc_status",
      "status": "PASS",
      "message": "clean"
    }
  ]
}`,
			wantNode: "node-2",
			wantLen:  1,
		},
		{
			name:    "no JSON at all",
			input:   "some random log line\nanother line\n",
			wantErr: true,
		},
		{
			name:    "empty input",
			input:   "",
			wantErr: true,
		},
		{
			name: "truncated JSON",
			input: `[PASS] check: ok
{
  "node": "n1",
  "results": [`,
			wantErr: true,
		},
		{
			name: "JSON followed by stderr lines",
			input: `Platform config: AKS
[FAIL] gpu_hardware/gpu_driver_version: nvidia-smi failed: exit status 12
[SKIP] gpu_hardware/gpu_ecc_status: nvidia-smi ECC query failed: exit status 12
[PASS] networking_rdma/rdma_devices_detected: 1 RDMA device(s) found: mlx5_0
{
  "node": "aks-gpupool-vmss000015",
  "timestamp": "2026-03-12T18:21:55Z",
  "results": [
    {
      "category": "gpu_hardware",
      "name": "gpu_driver_version",
      "status": "FAIL",
      "message": "nvidia-smi failed: exit status 12"
    }
  ]
}
Validation failed: one or more checks reported FAIL
Waiting for controller to collect results...`,
			wantNode: "aks-gpupool-vmss000015",
			wantLen:  1,
		},
		{
			name: "JSON with single result",
			input: `Platform config: aks
{
  "node": "aks-gpu-0",
  "timestamp": "2024-06-15T12:00:00Z",
  "results": [
    {
      "category": "gpu_hardware",
      "name": "gpu_driver_version",
      "status": "PASS",
      "message": "NVIDIA driver: 535.129.03, CUDA: 12.2, GPU: NVIDIA A100-SXM4-80GB (81920 MiB), 8 GPU(s)",
      "details": {
        "driver_version": "535.129.03",
        "gpu_count": 8
      }
    }
  ]
}`,
			wantNode: "aks-gpu-0",
			wantLen:  1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report, err := parseReport(strings.NewReader(tt.input))
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if report.Node != tt.wantNode {
				t.Errorf("Node = %q, want %q", report.Node, tt.wantNode)
			}
			if len(report.Results) != tt.wantLen {
				t.Errorf("got %d results, want %d", len(report.Results), tt.wantLen)
			}
		})
	}
}
