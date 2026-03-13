package gpu

import "testing"

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"535.129.03", "535.0", 1},
		{"535.0", "535.0", 0},
		{"530.0", "535.0", -1},
		{"535.129.03", "535.129.03", 0},
		{"550.0", "535.0", 1},
		{"535.0.1", "535.0.2", -1},
		{"1.2.3", "1.2.3", 0},
		{"1.2", "1.2.0", 0},
		{"1.10", "1.9", 1},
		{"", "535.0", -1},
		{"535.0", "", 1},
	}

	for _, tt := range tests {
		t.Run(tt.a+"_vs_"+tt.b, func(t *testing.T) {
			got := compareVersions(tt.a, tt.b)
			if got != tt.want {
				t.Errorf("compareVersions(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestParseDriverOutput(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    *driverInfo
		wantErr bool
	}{
		{
			name:  "single GPU",
			input: "535.129.03, 12.2, NVIDIA A100-SXM4-80GB, 81920",
			want: &driverInfo{
				DriverVersion: "535.129.03",
				CUDAVersion:   "12.2",
				GPUName:       "NVIDIA A100-SXM4-80GB",
				MemoryTotal:   "81920",
				GPUCount:      1,
			},
		},
		{
			name: "multiple GPUs",
			input: `535.129.03, 12.2, NVIDIA A100-SXM4-80GB, 81920
535.129.03, 12.2, NVIDIA A100-SXM4-80GB, 81920
535.129.03, 12.2, NVIDIA A100-SXM4-80GB, 81920
535.129.03, 12.2, NVIDIA A100-SXM4-80GB, 81920`,
			want: &driverInfo{
				DriverVersion: "535.129.03",
				CUDAVersion:   "12.2",
				GPUName:       "NVIDIA A100-SXM4-80GB",
				MemoryTotal:   "81920",
				GPUCount:      4,
			},
		},
		{
			name:  "H100 GPU",
			input: "550.54.15, 12.4, NVIDIA H100 80GB HBM3, 81559",
			want: &driverInfo{
				DriverVersion: "550.54.15",
				CUDAVersion:   "12.4",
				GPUName:       "NVIDIA H100 80GB HBM3",
				MemoryTotal:   "81559",
				GPUCount:      1,
			},
		},
		{
			name:    "empty output",
			input:   "",
			wantErr: true,
		},
		{
			name:    "too few fields",
			input:   "535.129.03, 12.2",
			wantErr: true,
		},
		{
			name:    "malformed CSV",
			input:   "\"unterminated quote",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseDriverOutput(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.DriverVersion != tt.want.DriverVersion {
				t.Errorf("DriverVersion = %q, want %q", got.DriverVersion, tt.want.DriverVersion)
			}
			if got.CUDAVersion != tt.want.CUDAVersion {
				t.Errorf("CUDAVersion = %q, want %q", got.CUDAVersion, tt.want.CUDAVersion)
			}
			if got.GPUName != tt.want.GPUName {
				t.Errorf("GPUName = %q, want %q", got.GPUName, tt.want.GPUName)
			}
			if got.MemoryTotal != tt.want.MemoryTotal {
				t.Errorf("MemoryTotal = %q, want %q", got.MemoryTotal, tt.want.MemoryTotal)
			}
			if got.GPUCount != tt.want.GPUCount {
				t.Errorf("GPUCount = %d, want %d", got.GPUCount, tt.want.GPUCount)
			}
		})
	}
}
