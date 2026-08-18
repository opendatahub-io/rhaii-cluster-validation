package networking

import (
	"fmt"
	"testing"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
)

func TestIperfJobParseResult(t *testing.T) {
	tests := []struct {
		name       string
		pass       float64
		warn       float64
		bps        float64
		wantStatus checks.Status
	}{
		{"above pass", 5.0, 1.0, 10e9, checks.StatusPass},  // 10 Gbps >= 5 pass
		{"at pass", 5.0, 1.0, 5e9, checks.StatusPass},      // 5 Gbps >= 5 pass
		{"warn range", 5.0, 1.0, 3e9, checks.StatusWarn},   // 3 Gbps >= 1 warn, < 5 pass
		{"at warn", 5.0, 1.0, 1e9, checks.StatusWarn},      // 1 Gbps >= 1 warn
		{"below warn", 5.0, 1.0, 0.5e9, checks.StatusFail}, // 0.5 Gbps < 1 warn
		{"zero bandwidth", 5.0, 1.0, 0, checks.StatusFail},
		{"high bandwidth", 100.0, 10.0, 200e9, checks.StatusPass},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := NewIperfJob(tt.pass, tt.warn, nil)
			json := `{"end":{"sum_sent":{"bits_per_second":` +
				formatFloat(tt.bps) + `,"retransmits":0}}}`

			result, err := job.ParseResult(json)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result.Status != tt.wantStatus {
				t.Errorf("Status = %q, want %q (message: %s)", result.Status, tt.wantStatus, result.Message)
			}
		})
	}
}

func TestIperfJobParseResultInvalid(t *testing.T) {
	job := NewIperfJob(5.0, 1.0, nil)
	tests := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"not json", "this is not json"},
		{"incomplete", `{"end":`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := job.ParseResult(tt.input)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

func formatFloat(f float64) string {
	return fmt.Sprintf("%f", f)
}
