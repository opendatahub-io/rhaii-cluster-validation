package networking

import (
	"fmt"
	"testing"
)

func TestParseIperfOutput(t *testing.T) {
	tests := []struct {
		name            string
		bitsPerSecond   float64
		retransmits     int
		wantGbps        float64
		wantRetransmits int
	}{
		{
			name:            "100 Gbps",
			bitsPerSecond:   100e9,
			retransmits:     0,
			wantGbps:        100.0,
			wantRetransmits: 0,
		},
		{
			name:            "25 Gbps with retransmits",
			bitsPerSecond:   25e9,
			retransmits:     42,
			wantGbps:        25.0,
			wantRetransmits: 42,
		},
		{
			name:            "1 Gbps",
			bitsPerSecond:   1e9,
			retransmits:     0,
			wantGbps:        1.0,
			wantRetransmits: 0,
		},
		{
			name:            "zero bandwidth",
			bitsPerSecond:   0,
			retransmits:     0,
			wantGbps:        0.0,
			wantRetransmits: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			json := fmt.Sprintf(`{"end":{"sum_sent":{"bits_per_second":%f,"retransmits":%d}}}`,
				tt.bitsPerSecond, tt.retransmits)

			got, err := parseIperfOutput([]byte(json))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Gbps != tt.wantGbps {
				t.Errorf("Gbps = %f, want %f", got.Gbps, tt.wantGbps)
			}
			if got.Retransmits != tt.wantRetransmits {
				t.Errorf("Retransmits = %d, want %d", got.Retransmits, tt.wantRetransmits)
			}
		})
	}
}

func TestParseIperfOutputInvalid(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"not JSON", "this is not json"},
		{"incomplete JSON", `{"end":`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseIperfOutput([]byte(tt.input))
			if err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

func TestBandwidthThresholdLogic(t *testing.T) {
	// Verify the threshold classification: >= threshold = PASS, >= 40% = WARN, < 40% = FAIL
	threshold := 25.0
	tests := []struct {
		gbps       float64
		wantStatus string
	}{
		{30.0, "pass"},
		{25.0, "pass"},
		{15.0, "warn"},  // 60% of 25 = pass threshold, but 15 >= 10 (40%)
		{10.0, "warn"},  // exactly 40%
		{9.9, "fail"},   // below 40%
		{0.0, "fail"},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("%.1f_gbps", tt.gbps), func(t *testing.T) {
			var status string
			switch {
			case tt.gbps >= threshold:
				status = "pass"
			case tt.gbps >= threshold*0.4:
				status = "warn"
			default:
				status = "fail"
			}
			if status != tt.wantStatus {
				t.Errorf("%.1f Gbps: got %s, want %s", tt.gbps, status, tt.wantStatus)
			}
		})
	}
}
