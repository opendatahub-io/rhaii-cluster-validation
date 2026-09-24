package gpu

import (
	"math"
	"testing"
)

// healthyAllReduceOutput is a trimmed but format-faithful capture from a real
// GB200 NVL4 run of `all_reduce_perf -b 8 -e 8G -f 2 -g 4` (data-correct,
// switchless direct-NVLink mesh, peak in-place busbw 605.85 GB/s).
const healthyAllReduceOutput = `# nccl-tests version 2.20.0 (b4d5bee) nccl-headers=23102 nccl-library=23102
# Collective test starting: all_reduce_perf
# nThread 1 nGpus 4 minBytes 8 maxBytes 8589934592 step: 2(factor) warmup iters: 1 iters: 20 agg iters: 1 validation: 1 graph: 0
#
#                                                              out-of-place                       in-place
#       size         count      type   redop    root     time   algbw   busbw  #wrong     time   algbw   busbw  #wrong
#        (B)    (elements)                               (us)  (GB/s)  (GB/s)             (us)  (GB/s)  (GB/s)
           8             2     float     sum      -1    55.18    0.00    0.00       0    38.83    0.00    0.00       0
     1048576        262144     float     sum      -1    35.39   29.63   44.44       0    34.44   30.45   45.68       0
   268435456      67108864     float     sum      -1   713.27  376.34  564.51       0   713.56  376.19  564.29       0
  8589934592    2147483648     float     sum      -1  21286.0  403.55  605.32       0  21267.4  403.90  605.85       0
# Out of bounds values : 0 OK
# Avg bus bandwidth    : 191.494
#
# Collective test concluded: all_reduce_perf
`

func floatEq(a, b float64) bool { return math.Abs(a-b) < 0.001 }

func TestParseAllReduceOutput_Healthy(t *testing.T) {
	res, err := parseAllReduceOutput(healthyAllReduceOutput)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !floatEq(res.PeakBusBWGBps, 605.85) {
		t.Errorf("peak busbw = %.2f, want 605.85", res.PeakBusBWGBps)
	}
	if res.WrongCount != 0 {
		t.Errorf("wrong count = %d, want 0", res.WrongCount)
	}
	if !res.OutOfBoundsOK {
		t.Error("out-of-bounds OK = false, want true")
	}
}

func TestParseAllReduceOutput_DataErrors(t *testing.T) {
	// A run where the in-place column reports mismatched elements and the
	// summary is not OK — interconnect corrupting data.
	const corrupt = `#       size         count      type   redop    root     time   algbw   busbw  #wrong     time   algbw   busbw  #wrong
   268435456      67108864     float     sum      -1   713.27  376.34  564.51       0   713.56  376.19  564.29       7
# Out of bounds values : 7 FAILED
`
	res, err := parseAllReduceOutput(corrupt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.WrongCount != 7 {
		t.Errorf("wrong count = %d, want 7", res.WrongCount)
	}
	if res.OutOfBoundsOK {
		t.Error("out-of-bounds OK = true, want false")
	}
}

func TestParseAllReduceOutput_NoDataRows(t *testing.T) {
	const headerOnly = `# nccl-tests version 2.20.0
# some error occurred before any data
`
	if _, err := parseAllReduceOutput(headerOnly); err == nil {
		t.Error("expected error for output with no data rows, got nil")
	}
}

func TestParseAllReduceOutput_MissingOutOfBounds(t *testing.T) {
	// Truncated output: a data row but no summary line (e.g. process killed).
	const truncated = `           8             2     float     sum      -1    55.18    0.00    0.00       0    38.83    0.00    0.00       0
`
	if _, err := parseAllReduceOutput(truncated); err == nil {
		t.Error("expected error for output missing out-of-bounds summary, got nil")
	}
}

func TestParseAllReduceOutput_PicksMaxAcrossColumns(t *testing.T) {
	// Peak may appear in the out-of-place column; ensure both are considered.
	const oopPeak = `   268435456      67108864     float     sum      -1   713.27  376.34  700.10       0   713.56  376.19  564.29       0
# Out of bounds values : 0 OK
`
	res, err := parseAllReduceOutput(oopPeak)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !floatEq(res.PeakBusBWGBps, 700.10) {
		t.Errorf("peak busbw = %.2f, want 700.10", res.PeakBusBWGBps)
	}
}

func TestParseGPUList(t *testing.T) {
	const out = `GPU 0: NVIDIA GB200 (UUID: GPU-aaaa)
GPU 1: NVIDIA GB200 (UUID: GPU-bbbb)
GPU 2: NVIDIA GB200 (UUID: GPU-cccc)
GPU 3: NVIDIA GB200 (UUID: GPU-dddd)`
	names := parseGPUList(out)
	if len(names) != 4 {
		t.Fatalf("parsed %d GPUs, want 4", len(names))
	}
	for i, n := range names {
		if n != "NVIDIA GB200" {
			t.Errorf("GPU %d name = %q, want %q", i, n, "NVIDIA GB200")
		}
	}
}

func TestParseGPUList_Empty(t *testing.T) {
	if names := parseGPUList(""); len(names) != 0 {
		t.Errorf("parseGPUList(\"\") = %v, want empty", names)
	}
}

func TestIsGB200System(t *testing.T) {
	tests := []struct {
		name  string
		names []string
		want  bool
	}{
		{"gb200 node", []string{"NVIDIA GB200", "NVIDIA GB200"}, true},
		{"h100 node", []string{"NVIDIA H100 80GB HBM3", "NVIDIA H100 80GB HBM3"}, false},
		{"a100 node", []string{"NVIDIA A100-SXM4-80GB"}, false},
		{"no gpus", nil, false},
		{"mixed with gb200", []string{"NVIDIA H100", "NVIDIA GB200"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isGB200System(tt.names); got != tt.want {
				t.Errorf("isGB200System(%v) = %v, want %v", tt.names, got, tt.want)
			}
		})
	}
}

func TestLastLines(t *testing.T) {
	const in = "a\nb\nc\nd\ne\n"
	if got := lastLines(in, 2); got != "d\ne" {
		t.Errorf("lastLines(_, 2) = %q, want %q", got, "d\ne")
	}
	if got := lastLines("only\n", 5); got != "only" {
		t.Errorf("lastLines fewer-than-n = %q, want %q", got, "only")
	}
}
