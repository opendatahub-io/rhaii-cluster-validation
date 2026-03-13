package networking

import (
	"fmt"
	"testing"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
)

func TestIperfJobParseResult(t *testing.T) {
	tests := []struct {
		name       string
		threshold  float64
		bps        float64
		wantStatus checks.Status
	}{
		{"above threshold", 25.0, 30e9, checks.StatusPass},
		{"at threshold", 25.0, 25e9, checks.StatusPass},
		{"warn range", 25.0, 15e9, checks.StatusWarn},  // 60% of 25 = above 40%
		{"at 40%", 25.0, 10e9, checks.StatusWarn},       // exactly 40%
		{"below 40%", 25.0, 9e9, checks.StatusFail},
		{"zero bandwidth", 25.0, 0, checks.StatusFail},
		{"high bandwidth", 100.0, 200e9, checks.StatusPass},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := NewIperfJob(tt.threshold, nil)
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
	job := NewIperfJob(25.0, nil)
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
