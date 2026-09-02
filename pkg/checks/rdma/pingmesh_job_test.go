package rdma

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/config"
)

func TestParseResult(t *testing.T) {
	tests := []struct {
		name       string
		logs       string
		wantPassed int
		wantTotal  int
		wantStatus checks.Status
		wantErr    bool
	}{
		{
			name:       "all pass",
			logs:       `{"server_node":"nodeA","client_node":"nodeB","results":[{"src_dev":"ibp0","dst_dev":"ibp0","pass":true},{"src_dev":"ibp1","dst_dev":"ibp1","pass":true}]}`,
			wantPassed: 2,
			wantTotal:  2,
			wantStatus: checks.StatusPass,
		},
		{
			name:       "partial failure",
			logs:       `{"server_node":"nodeA","client_node":"nodeB","results":[{"src_dev":"ibp0","dst_dev":"ibp0","pass":true},{"src_dev":"ibp1","dst_dev":"ibp0","pass":false,"error":"timeout"}]}`,
			wantPassed: 1,
			wantTotal:  2,
			wantStatus: checks.StatusFail,
		},
		{
			name:       "all fail",
			logs:       `{"server_node":"a","client_node":"b","results":[{"src_dev":"mlx5_0","dst_dev":"mlx5_0","pass":false,"error":"connect refused"}]}`,
			wantPassed: 0,
			wantTotal:  1,
			wantStatus: checks.StatusFail,
		},
		{
			name:       "empty results array",
			logs:       `{"server_node":"a","client_node":"b","results":[]}`,
			wantPassed: 0,
			wantTotal:  0,
			wantStatus: checks.StatusFail,
		},
		{
			name:       "with leading noise",
			logs:       "some startup log\n" + `{"server_node":"x","client_node":"y","results":[{"src_dev":"ibp0","dst_dev":"ibp0","pass":true}]}`,
			wantPassed: 1,
			wantTotal:  1,
			wantStatus: checks.StatusPass,
		},
		{
			name:       "with trailing percent sign",
			logs:       `{"server_node":"x","client_node":"y","results":[{"src_dev":"ibp0","dst_dev":"ibp0","pass":true}]}` + "%",
			wantPassed: 1,
			wantTotal:  1,
			wantStatus: checks.StatusPass,
		},
		{
			name:    "no JSON",
			logs:    "just some text\n",
			wantErr: true,
		},
		{
			name:    "empty",
			logs:    "",
			wantErr: true,
		},
		{
			name:    "malformed JSON",
			logs:    `{"server_node":"a","results":[{broken`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			j := &PingMeshJob{}
			result, err := j.ParseResult(tt.logs)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result.Status != tt.wantStatus {
				t.Errorf("Status = %q, want %q", result.Status, tt.wantStatus)
			}
			results, ok := result.Details.([]PingMeshPairResult)
			if !ok {
				t.Fatalf("Details type = %T, want []PingMeshPairResult", result.Details)
			}
			if len(results) != tt.wantTotal {
				t.Errorf("total = %d, want %d", len(results), tt.wantTotal)
			}
			passed := 0
			for _, r := range results {
				if r.Pass {
					passed++
				}
			}
			if passed != tt.wantPassed {
				t.Errorf("passed = %d, want %d", passed, tt.wantPassed)
			}
		})
	}
}

func TestParseResultWrappedFormat(t *testing.T) {
	// Validates that ParseResult correctly handles the wrapped JSON format
	// with server_node/client_node fields (node names are consumed during
	// parsing but not exposed on JobResult — they're used by the controller
	// for classification via the raw client pod logs).
	logs := `{"server_node":"gpu-node-1","client_node":"gpu-node-2","results":[{"src_dev":"ibp0","dst_dev":"ibp0","pass":true}]}`
	j := &PingMeshJob{}
	result, err := j.ParseResult(logs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	results, ok := result.Details.([]PingMeshPairResult)
	if !ok {
		t.Fatalf("Details type = %T, want []PingMeshPairResult", result.Details)
	}
	if len(results) != 1 {
		t.Errorf("result count = %d, want 1", len(results))
	}
	if result.Status != checks.StatusPass {
		t.Errorf("Status = %q, want PASS", result.Status)
	}
}

func TestValidDeviceCount(t *testing.T) {
	j := &PingMeshJob{}
	if got := j.validDeviceCount([]string{"ibp0", "ibp1", "mlx5_0"}); got != 3 {
		t.Errorf("validDeviceCount = %d, want 3", got)
	}
	if got := j.validDeviceCount([]string{"ibp0", "../etc/passwd", "mlx5_0"}); got != 2 {
		t.Errorf("validDeviceCount with invalid = %d, want 2", got)
	}
	if got := j.validDeviceCount(nil); got != 0 {
		t.Errorf("validDeviceCount(nil) = %d, want 0", got)
	}
}

func TestNewPingMeshJobRejectsNoValidDevices(t *testing.T) {
	valid := []string{"ibp0"}
	tests := []struct {
		name       string
		serverDevs []string
		clientDevs []string
	}{
		{"empty server", nil, valid},
		{"invalid client", valid, []string{"../etc/passwd"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			j := NewPingMeshJob("a", "b", tt.serverDevs, tt.clientDevs, config.RDMATypeSRD, -1, 3, 10)
			if err := j.ValidateDevices(); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestPingMeshJobFiltersMixedDeviceLists(t *testing.T) {
	rawServer := []string{"ibp0", "../bad", "ibp1"}
	rawClient := []string{"ibp0", "bad dev", "ibp1"}
	j := NewPingMeshJob("a", "b", rawServer, rawClient, config.RDMATypeSRD, -1, 3, 10)
	if err := j.ValidateDevices(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(j.ServerDevices) != 2 || len(j.ClientDevices) != 2 {
		t.Fatalf("filtered devices = %v / %v, want 2 each", j.ServerDevices, j.ClientDevices)
	}

	serverScript := j.serverScript()[2]
	clientScript := j.clientScript("10.0.0.1")[2]
	// Server inner loop must use filtered client count (2 → seq 0 1), not raw (3 → seq 0 2).
	if !strings.Contains(serverScript, "for cslot in $(seq 0 1)") {
		t.Errorf("server script missing filtered client loop bound:\n%s", serverScript)
	}
	if strings.Contains(serverScript, "for cslot in $(seq 0 2)") {
		t.Error("server script still uses raw client device count")
	}
	if !strings.Contains(clientScript, `CDEVS=("ibp0" "ibp1")`) {
		t.Errorf("client script missing filtered CDEVS array:\n%s", clientScript)
	}
}

func TestServerTimeout(t *testing.T) {
	j := NewPingMeshJob("a", "b",
		[]string{"ibp0", "ibp1"},
		[]string{"ibp0", "ibp1"},
		config.RDMATypeIB, -1, 1, 10)
	// 2 server × 2 client = 4 tests, 4*10 + 30 = 70
	if got := j.serverTimeout(); got != 70 {
		t.Errorf("serverTimeout = %d, want 70", got)
	}
}

func TestPingMeshJobScripts(t *testing.T) {
	serverDevs := []string{"ibp0", "ibp1"}
	clientDevs := []string{"ibp0", "ibp1"}

	t.Run("ibv server uses ibv_rc_pingpong", func(t *testing.T) {
		j := NewPingMeshJob("a", "b", serverDevs, clientDevs, config.RDMATypeIB, -1, 3, 10)
		script := j.serverScript()[2]
		if !strings.Contains(script, "ibv_rc_pingpong") {
			t.Error("expected ibv_rc_pingpong in IB server script")
		}
		if strings.Contains(script, "fi_rdm_pingpong") {
			t.Error("unexpected fi_rdm_pingpong in IB server script")
		}
		if !strings.Contains(script, "-n 3") {
			t.Error("expected -n 3 in IB server script")
		}
	})

	t.Run("ibv client uses ibv_rc_pingpong", func(t *testing.T) {
		j := NewPingMeshJob("a", "b", serverDevs, clientDevs, config.RDMATypeIB, -1, 3, 10)
		script := j.clientScript("10.0.0.1")[2]
		if !strings.Contains(script, "ibv_rc_pingpong") {
			t.Error("expected ibv_rc_pingpong in IB client script")
		}
		if strings.Contains(script, "fi_rdm_pingpong") {
			t.Error("unexpected fi_rdm_pingpong in IB client script")
		}
		if !strings.Contains(script, "-n 3 10.0.0.1") {
			t.Error("expected -n 3 and server IP in IB client script")
		}
	})

	t.Run("srd server uses fi_rdm_pingpong", func(t *testing.T) {
		j := NewPingMeshJob("a", "b", serverDevs, clientDevs, config.RDMATypeSRD, -1, 3, 10)
		script := j.serverScript()[2]
		if !strings.Contains(script, "fi_rdm_pingpong") {
			t.Error("expected fi_rdm_pingpong in SRD server script")
		}
		if strings.Contains(script, "ibv_rc_pingpong") {
			t.Error("unexpected ibv_rc_pingpong in SRD server script")
		}
		if !strings.Contains(script, "-p efa") {
			t.Error("expected -p efa provider in SRD server script")
		}
		if !strings.Contains(script, "FI_EFA_IFACE=$sdev") {
			t.Error("expected FI_EFA_IFACE in SRD server script")
		}
		if !strings.Contains(script, "-E=$((18515 + idx))") {
			t.Error("expected OOB port -E in SRD server script")
		}
		if !strings.Contains(script, "-S 64") {
			t.Error("expected -S 64 message size in SRD server script")
		}
		if !strings.Contains(script, "-I 3") {
			t.Error("expected -I 3 in SRD server script")
		}
		if strings.Contains(script, "find_rocev2_gid") {
			t.Error("SRD server script should not use GID discovery")
		}
	})

	t.Run("srd client uses fi_rdm_pingpong", func(t *testing.T) {
		j := NewPingMeshJob("a", "b", serverDevs, clientDevs, config.RDMATypeSRD, -1, 3, 10)
		script := j.clientScript("10.0.0.1")[2]
		if !strings.Contains(script, "fi_rdm_pingpong") {
			t.Error("expected fi_rdm_pingpong in SRD client script")
		}
		if strings.Contains(script, "ibv_rc_pingpong") {
			t.Error("unexpected ibv_rc_pingpong in SRD client script")
		}
		if !strings.Contains(script, "-p efa") {
			t.Error("expected -p efa provider in SRD client script")
		}
		if !strings.Contains(script, "FI_EFA_IFACE=$cdev") {
			t.Error("expected FI_EFA_IFACE per client NIC in SRD client script")
		}
		if !strings.Contains(script, "SDEVS=(") || !strings.Contains(script, "CDEVS=(") {
			t.Error("expected device arrays in SRD client script")
		}
		if !strings.Contains(script, "-E=$((18515 + idx))") {
			t.Error("expected OOB port -E in SRD client script")
		}
		if !strings.Contains(script, "-S 64") {
			t.Error("expected -S 64 message size in SRD client script")
		}
		if !strings.Contains(script, "-I 3 10.0.0.1") {
			t.Error("expected -I 3 and server IP in SRD client script")
		}
		if !strings.Contains(script, "wait\n") {
			t.Error("expected parallel client with wait in SRD client script")
		}
	})

	t.Run("default iterations is 3", func(t *testing.T) {
		j := NewPingMeshJob("a", "b", serverDevs, clientDevs, config.RDMATypeSRD, -1, 0, 10)
		if j.Iterations != 3 {
			t.Errorf("Iterations = %d, want 3 (default)", j.Iterations)
		}
		script := j.clientScript("10.0.0.1")[2]
		if !strings.Contains(script, "-I 3") {
			t.Error("expected default -I 3 in SRD client script")
		}
	})
}

func TestClientScriptSize32x32(t *testing.T) {
	devs := make([]string, 32)
	for i := range devs {
		devs[i] = fmt.Sprintf("rdmap%ds0", 79+i)
	}
	j := NewPingMeshJob("server", "client", devs, devs, config.RDMATypeSRD, -1, 3, 10)
	script := j.clientScript("10.0.0.1")[2]
	const maxScriptBytes = 128 * 1024 // stay well under typical ARG_MAX (~2MB)
	if len(script) > maxScriptBytes {
		t.Errorf("32x32 client script = %d bytes, want <= %d (unrolled scripts hit ARG_MAX)", len(script), maxScriptBytes)
	}
	t.Logf("32x32 client script size: %d bytes", len(script))
}

// TestDump32x32Scripts writes generated pingmesh scripts to /tmp for manual pod testing.
// Usage: DUMP_PINGMESH_SCRIPTS=1 DEVS="rdmap79s0 ..." SERVER_IP=10.0.0.1 go test -run TestDump32x32Scripts -count=1
func TestDump32x32Scripts(t *testing.T) {
	if os.Getenv("DUMP_PINGMESH_SCRIPTS") == "" {
		t.Skip("set DUMP_PINGMESH_SCRIPTS=1 to generate scripts")
	}
	devs := strings.Fields(os.Getenv("DEVS"))
	if len(devs) == 0 {
		t.Fatal("DEVS env required")
	}
	serverIP := os.Getenv("SERVER_IP")
	if serverIP == "" {
		t.Fatal("SERVER_IP env required")
	}
	j := NewPingMeshJob("server-test", "client-test", devs, devs, config.RDMATypeSRD, -1, 3, 10)
	serverScript := j.serverScript()[2]
	clientScript := j.clientScript(serverIP)[2]
	if err := os.WriteFile("/tmp/pingmesh_server.sh", []byte(serverScript), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("/tmp/pingmesh_client.sh", []byte(clientScript), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote /tmp/pingmesh_server.sh (%d bytes, %d devs)", len(serverScript), len(devs))
	t.Logf("wrote /tmp/pingmesh_client.sh (%d bytes)", len(clientScript))
}
