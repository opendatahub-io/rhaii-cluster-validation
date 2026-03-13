package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
)

// mockCheck is a Check that returns a canned Result.
type mockCheck struct {
	name     string
	category string
	result   checks.Result
}

func (m *mockCheck) Name() string     { return m.name }
func (m *mockCheck) Category() string { return m.category }
func (m *mockCheck) Run(_ context.Context) checks.Result {
	return m.result
}

func TestRunnerOutputJSON(t *testing.T) {
	var output bytes.Buffer
	r := New("test-node", &output)
	r.log = &bytes.Buffer{} // suppress stderr

	r.AddCheck(&mockCheck{
		name:     "check_one",
		category: "cat_a",
		result: checks.Result{
			Node:     "test-node",
			Category: "cat_a",
			Name:     "check_one",
			Status:   checks.StatusPass,
			Message:  "all good",
		},
	})
	r.AddCheck(&mockCheck{
		name:     "check_two",
		category: "cat_b",
		result: checks.Result{
			Node:        "test-node",
			Category:    "cat_b",
			Name:        "check_two",
			Status:      checks.StatusFail,
			Message:     "something broke",
			Remediation: "fix it",
		},
	})

	_, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	var report checks.NodeReport
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatalf("failed to parse JSON output: %v\nraw: %s", err, output.String())
	}

	if report.Node != "test-node" {
		t.Errorf("Node = %q, want %q", report.Node, "test-node")
	}
	if report.Timestamp.IsZero() {
		t.Error("Timestamp should not be zero")
	}
	if len(report.Results) != 2 {
		t.Fatalf("got %d results, want 2", len(report.Results))
	}
	if report.Results[0].Status != checks.StatusPass {
		t.Errorf("result[0].Status = %q, want %q", report.Results[0].Status, checks.StatusPass)
	}
	if report.Results[1].Status != checks.StatusFail {
		t.Errorf("result[1].Status = %q, want %q", report.Results[1].Status, checks.StatusFail)
	}
	if report.Results[1].Remediation != "fix it" {
		t.Errorf("result[1].Remediation = %q, want %q", report.Results[1].Remediation, "fix it")
	}
}

func TestRunnerNoChecks(t *testing.T) {
	var output bytes.Buffer
	r := New("empty-node", &output)
	r.log = &bytes.Buffer{}

	_, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	var report checks.NodeReport
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatalf("failed to parse JSON: %v", err)
	}
	if report.Node != "empty-node" {
		t.Errorf("Node = %q, want %q", report.Node, "empty-node")
	}
	if report.Results != nil {
		t.Errorf("expected nil Results, got %v", report.Results)
	}
}

func TestRunnerProgressToLog(t *testing.T) {
	var output, logBuf bytes.Buffer
	r := New("node1", &output)
	r.log = &logBuf

	r.AddCheck(&mockCheck{
		name:     "test_check",
		category: "test_cat",
		result: checks.Result{
			Node:     "node1",
			Category: "test_cat",
			Name:     "test_check",
			Status:   checks.StatusWarn,
			Message:  "heads up",
		},
	})

	_, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	logStr := logBuf.String()
	if logStr == "" {
		t.Error("expected progress output in log, got empty")
	}
	// Verify JSON is NOT in log (should only be in output)
	if json.Valid([]byte(logStr)) {
		t.Error("log should contain progress lines, not JSON")
	}
	// Verify JSON IS in output
	if !json.Valid(output.Bytes()) {
		t.Errorf("output should be valid JSON, got: %s", output.String())
	}
}

func TestHasFailures(t *testing.T) {
	tests := []struct {
		name    string
		results []checks.Result
		want    bool
	}{
		{
			name:    "no results",
			results: nil,
			want:    false,
		},
		{
			name: "all pass",
			results: []checks.Result{
				{Status: checks.StatusPass},
				{Status: checks.StatusPass},
			},
			want: false,
		},
		{
			name: "one fail",
			results: []checks.Result{
				{Status: checks.StatusPass},
				{Status: checks.StatusFail},
			},
			want: true,
		},
		{
			name: "warn is not fail",
			results: []checks.Result{
				{Status: checks.StatusWarn},
				{Status: checks.StatusSkip},
			},
			want: false,
		},
		{
			name: "all fail",
			results: []checks.Result{
				{Status: checks.StatusFail},
				{Status: checks.StatusFail},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report := checks.NodeReport{Results: tt.results}
			if got := HasFailures(report); got != tt.want {
				t.Errorf("HasFailures() = %v, want %v", got, tt.want)
			}
		})
	}
}
