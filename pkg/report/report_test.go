package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks/rdma"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/jobrunner"
)

func TestCountStatuses(t *testing.T) {
	clusterResults := []checks.Result{
		{Status: checks.StatusPass},
		{Status: checks.StatusFail},
	}
	nodeReports := []checks.NodeReport{
		{Results: []checks.Result{{Status: checks.StatusPass}, {Status: checks.StatusWarn}}},
	}
	jobResults := []jobrunner.JobResult{
		{Status: checks.StatusPass},
		{Status: checks.StatusSkip}, // job results don't count SKIP, matching legacy behavior
	}
	pingmesh := &rdma.PingMeshReport{
		Summary: map[string]rdma.PingMeshCheckSummary{
			"rdma_conn_rail": {Status: checks.StatusFail},
		},
	}

	pass, warn, fail, skip := CountStatuses(clusterResults, nodeReports, jobResults, pingmesh)
	if pass != 3 || warn != 1 || fail != 2 || skip != 1 {
		t.Errorf("CountStatuses() = pass=%d warn=%d fail=%d skip=%d, want 3/1/2/1", pass, warn, fail, skip)
	}
}

func TestCountStatuses_NilPingmesh(t *testing.T) {
	pass, warn, fail, skip := CountStatuses(nil, nil, nil, nil)
	if pass != 0 || warn != 0 || fail != 0 || skip != 0 {
		t.Errorf("expected all zero counts, got %d/%d/%d/%d", pass, warn, fail, skip)
	}
}

func TestReadinessStatus(t *testing.T) {
	tests := []struct {
		fail, warn int
		want       string
	}{
		{0, 0, "READY"},
		{0, 2, "READY (with warnings)"},
		{1, 0, "NOT READY"},
		{1, 1, "NOT READY"},
	}
	for _, tt := range tests {
		if got := ReadinessStatus(tt.fail, tt.warn); got != tt.want {
			t.Errorf("ReadinessStatus(%d, %d) = %q, want %q", tt.fail, tt.warn, got, tt.want)
		}
	}
}

func TestBuild(t *testing.T) {
	r := Build("ocp", "", []checks.Result{{Status: checks.StatusFail}}, nil, nil, nil)
	if r.Platform != "ocp" {
		t.Errorf("Platform = %q, want ocp", r.Platform)
	}
	if r.Summary["fail"] != 1 {
		t.Errorf("Summary[fail] = %d, want 1", r.Summary["fail"])
	}
	if r.Status != "NOT READY" {
		t.Errorf("Status = %q, want NOT READY", r.Status)
	}
}

func TestMergePreserving_FillsMissingSections(t *testing.T) {
	previous := Report{
		Nodes:      []checks.NodeReport{{Node: "node-1", Results: []checks.Result{{Status: checks.StatusFail}}}},
		JobResults: []jobrunner.JobResult{{Status: checks.StatusPass}},
		Pingmesh: &rdma.PingMeshReport{
			Summary: map[string]rdma.PingMeshCheckSummary{"rdma_conn_rail": {Status: checks.StatusPass}},
		},
	}
	// current run (e.g. rdma-ping) produced no Nodes/JobResults of its own
	current := Report{
		ClusterChecks: []checks.Result{{Status: checks.StatusPass}},
	}

	merged := MergePreserving(current, previous)

	if len(merged.Nodes) != 1 {
		t.Fatalf("expected Nodes preserved from previous report, got %d", len(merged.Nodes))
	}
	if len(merged.JobResults) != 1 {
		t.Fatalf("expected JobResults preserved from previous report, got %d", len(merged.JobResults))
	}
	if merged.Pingmesh == nil {
		t.Fatal("expected Pingmesh preserved from previous report")
	}
	// Summary/Status must be recomputed from merged data, not just carried over.
	if merged.Summary["fail"] != 1 {
		t.Errorf("Summary[fail] = %d, want 1 (from preserved failing node)", merged.Summary["fail"])
	}
	if merged.Status != "NOT READY" {
		t.Errorf("Status = %q, want NOT READY", merged.Status)
	}
}

func TestMergePreserving_DoesNotOverwriteCurrentData(t *testing.T) {
	previous := Report{
		Nodes: []checks.NodeReport{{Node: "old-node"}},
	}
	current := Report{
		Nodes: []checks.NodeReport{{Node: "new-node"}},
	}

	merged := MergePreserving(current, previous)

	if len(merged.Nodes) != 1 || merged.Nodes[0].Node != "new-node" {
		t.Errorf("expected current run's Nodes to take precedence, got %+v", merged.Nodes)
	}
}

func TestFormatTable_ContainsSummaryAndStoredHint(t *testing.T) {
	r := Build("aks", "", []checks.Result{{Category: "cat", Name: "check1", Status: checks.StatusPass, Message: "ok"}}, nil, nil, nil)

	var buf bytes.Buffer
	fail := FormatTable(&buf, "aks", r, "kubectl get cm example")

	out := buf.String()
	if fail {
		t.Error("FormatTable() returned true, want false (no FAIL results)")
	}
	if !strings.Contains(out, "Platform: aks") {
		t.Errorf("expected platform line, got:\n%s", out)
	}
	if !strings.Contains(out, "Summary: 1 PASS | 0 WARN | 0 FAIL | 0 SKIP") {
		t.Errorf("expected summary line, got:\n%s", out)
	}
	if !strings.Contains(out, "kubectl get cm example") {
		t.Errorf("expected stored-report hint, got:\n%s", out)
	}
}

func TestFormatTable_ReturnsTrueOnFailure(t *testing.T) {
	r := Build("aks", "", []checks.Result{{Status: checks.StatusFail}}, nil, nil, nil)
	var buf bytes.Buffer
	if !FormatTable(&buf, "aks", r, "") {
		t.Error("FormatTable() returned false, want true (FAIL result present)")
	}
	if !strings.Contains(buf.String(), "NOT READY") {
		t.Errorf("expected NOT READY status line, got:\n%s", buf.String())
	}
}

func TestFormatJSON(t *testing.T) {
	r := Build("eks", "", []checks.Result{{Status: checks.StatusWarn}}, nil, nil, nil)
	var buf bytes.Buffer
	fail := FormatJSON(&buf, r)
	if fail {
		t.Error("FormatJSON() returned true, want false (only WARN present)")
	}

	var decoded Report
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if decoded.Platform != "eks" {
		t.Errorf("decoded Platform = %q, want eks", decoded.Platform)
	}
}
