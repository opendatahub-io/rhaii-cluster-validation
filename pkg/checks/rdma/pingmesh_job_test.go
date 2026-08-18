package rdma

import (
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
			wantStatus: checks.StatusPass,
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
