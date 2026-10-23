package rdma

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/config"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/jobrunner"
)

func TestRailEnvKey(t *testing.T) {
	for in, want := range map[string]string{
		"openshift.io/p6rdma":   "PCIDEVICE_OPENSHIFT_IO_P6RDMA",
		"example.com/roce-rail": "PCIDEVICE_EXAMPLE_COM_ROCE-RAIL",
	} {
		if got := railEnvKey(in); got != want {
			t.Errorf("railEnvKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestResolveDevFunc runs the generated bash against the env the SR-IOV device
// plugin sets (captured from a live pod: PCIDEVICE_..._INFO with rdma_dev).
func TestResolveDevFunc(t *testing.T) {
	for _, bin := range []string{"bash", "jq"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed", bin)
		}
	}
	tests := []struct {
		name string
		env  []string
		want string
	}{
		{"one VF from _INFO", []string{`K_INFO={"0000:19:00.6":{"generic":{"deviceID":"0000:19:00.6"},"rdma":{"rdma_dev":"mlx5_18"}}}`, "K=0000:19:00.6"}, "mlx5_18"},
		{"two VFs is ambiguous", []string{`K_INFO={"a":{"rdma":{"rdma_dev":"mlx5_1"}},"b":{"rdma":{"rdma_dev":"mlx5_2"}}}`, "K=a,b"}, ""},
		{"unsafe device name rejected", []string{`K_INFO={"a":{"rdma":{"rdma_dev":"x; rm -rf /"}}}`}, ""},
		{"nothing allocated", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.Command("bash", "-c", resolveDevFunc()+"\nresolve_dev K")
			cmd.Env = append(os.Environ(), tt.env...)
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("bash failed: %v", err)
			}
			if got := string(bytes.TrimSpace(out)); got != tt.want {
				t.Fatalf("resolve_dev = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPingMeshRailScriptsResolveDevicesAtRuntime(t *testing.T) {
	j := NewPingMeshJob("a", "b", []string{"mlx5_0"}, []string{"mlx5_9"}, config.RDMATypeRoCE, -1, 3, 10)
	j.Rails = []string{"openshift.io/p2rdma", "openshift.io/p6rdma"}
	j.SetPodConfig(&jobrunner.PodConfig{})

	server := strings.Join(j.serverScript(), " ")
	client := strings.Join(j.clientScript("10.0.0.1"), " ")
	for _, script := range []string{server, client} {
		if strings.Contains(script, "mlx5_0") || strings.Contains(script, "mlx5_9") {
			t.Fatalf("stored device names must not be used in rail mode:\n%s", script)
		}
		if !strings.Contains(script, "resolve_dev PCIDEVICE_OPENSHIFT_IO_P6RDMA") {
			t.Fatalf("rail device not resolved at runtime:\n%s", script)
		}
	}
	if !strings.Contains(server, "export -f resolve_dev") {
		t.Error("server runs resolve_dev inside a nested bash, so it must be exported")
	}
	if n := strings.Count(client, "ibv_rc_pingpong"); n != 4 {
		t.Errorf("client probes %d rail pairs, want 2x2=4", n)
	}
	if !strings.Contains(client, `:openshift.io/p2rdma:openshift.io/p6rdma" >> /tmp/pm/results.txt`) {
		t.Error("results must carry the client and server rail")
	}
}

func TestClassifyPingMeshByRailName(t *testing.T) {
	// 6-GPU vs 8-GPU nodes: topology positions disagree, rail names do not.
	results := []PingMeshPairResult{
		{SrcDev: "mlx5_18", SrcRail: "openshift.io/p6rdma", DstRail: "openshift.io/p6rdma", Pass: true},
		{SrcDev: "mlx5_18", SrcRail: "openshift.io/p6rdma", DstRail: "openshift.io/p8rdma", Pass: true},
		{SrcDev: "mlx5_23", SrcRail: "openshift.io/p8rdma", DstRail: "openshift.io/p6rdma", Pass: false, Error: "timeout"},
	}
	pair := jobrunner.NodePair{Server: "a", Client: "b"}
	topo := map[string]*checks.NodeTopology{"a": {}, "b": {}}
	report, failures := ClassifyPingMeshResults(
		map[jobrunner.NodePair][]jobrunner.JobResult{pair: {{Details: results}}}, topo, &bytes.Buffer{})

	if got := report.Summary["rdma_conn_rail"]; got.Passed != 1 || got.Total != 1 {
		t.Errorf("rail = %d/%d, want 1/1", got.Passed, got.Total)
	}
	if got := report.Summary["rdma_conn_xrail"]; got.Passed != 1 || got.Total != 2 {
		t.Errorf("xrail = %d/%d, want 1/2", got.Passed, got.Total)
	}
	if len(failures.Failures) != 1 || failures.Failures[0].SrcRail != "openshift.io/p8rdma" {
		t.Errorf("failure must name its rails: %+v", failures.Failures)
	}
}
