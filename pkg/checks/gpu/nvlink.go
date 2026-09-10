package gpu

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
)

// allReducePerfBinary is the nccl-tests all-reduce benchmark. It is not shipped
// in the validator image today (see CLAUDE.md "Known Limitations"); when absent
// the check SKIPs and auto-activates once an NCCL-capable image provides it.
const allReducePerfBinary = "all_reduce_perf"

// nvlinkMaxBytes is the largest message size passed to all_reduce_perf. 1 GiB is
// large enough for busbw to plateau (bandwidth is flat well before this) while
// keeping the preflight to a few seconds.
const nvlinkMaxBytes = "1G"

// NVLinkCheck verifies intra-node GPU interconnect (NVLink) health by running an
// NCCL all-reduce across all local GPUs and inspecting both correctness and the
// achieved bus bandwidth. On NVLink-connected multi-GPU nodes (e.g. GB200 NVL4)
// a healthy result is data-correct with bus bandwidth in the hundreds of GB/s; a
// PCIe fallback or degraded fabric shows tens of GB/s, and a broken interconnect
// shows nonzero "wrong" counts.
type NVLinkCheck struct {
	nodeName string
	passGBps float64 // busbw >= passGBps => PASS (GB/s, gigabytes/sec, as reported by nccl-tests)
	warnGBps float64 // busbw >= warnGBps => WARN, below => FAIL
}

// NewNVLinkCheck creates an NVLink NCCL all-reduce health check. passGBps and
// warnGBps are peak bus-bandwidth thresholds in GB/s (gigabytes/sec) — the unit
// nccl-tests reports as "busbw", not gigabits.
func NewNVLinkCheck(nodeName string, passGBps, warnGBps float64) *NVLinkCheck {
	return &NVLinkCheck{
		nodeName: nodeName,
		passGBps: passGBps,
		warnGBps: warnGBps,
	}
}

func (c *NVLinkCheck) Name() string     { return "gpu_nvlink_nccl" }
func (c *NVLinkCheck) Category() string { return "gpu_hardware" }

func (c *NVLinkCheck) Run(ctx context.Context) checks.Result {
	r := checks.Result{
		Node:     c.nodeName,
		Category: c.Category(),
		Name:     c.Name(),
	}

	// NVLink is a multi-GPU interconnect: nothing to test with fewer than 2 GPUs.
	gpuCount, err := countGPUs(ctx)
	if err != nil {
		r.Status = checks.StatusSkip
		r.Message = fmt.Sprintf("could not enumerate GPUs, skipping NVLink test: %v", err)
		return r
	}
	if gpuCount < 2 {
		r.Status = checks.StatusSkip
		r.Message = fmt.Sprintf("NVLink test needs >= 2 GPUs, found %d", gpuCount)
		return r
	}

	// The nccl-tests binary is not present in the validator image yet; skip
	// cleanly rather than fail so this check activates automatically once an
	// NCCL-capable image ships it.
	binPath, err := exec.LookPath(allReducePerfBinary)
	if err != nil {
		r.Status = checks.StatusSkip
		r.Message = fmt.Sprintf("%s not found on PATH; NVLink NCCL test requires an NCCL-capable image", allReducePerfBinary)
		r.Remediation = "Run this check from an image that includes nccl-tests (all_reduce_perf)"
		return r
	}

	// all_reduce_perf -b 8 -e <max> -f 2 -g <gpuCount>: single process driving
	// all local GPUs, message sizes 8B..max doubling each step.
	output, err := exec.CommandContext(ctx, binPath,
		"-b", "8",
		"-e", nvlinkMaxBytes,
		"-f", "2",
		"-g", strconv.Itoa(gpuCount)).CombinedOutput()
	if err != nil {
		r.Status = checks.StatusFail
		r.Message = fmt.Sprintf("%s failed: %v", allReducePerfBinary, err)
		r.Remediation = "Check NVLink status (nvidia-smi nvlink -s), fabric manager, and GPU topology (nvidia-smi topo -m)"
		r.Details = map[string]any{"output": lastLines(string(output), 20)}
		return r
	}

	res, err := parseAllReduceOutput(string(output))
	if err != nil {
		r.Status = checks.StatusFail
		r.Message = fmt.Sprintf("could not parse %s output: %v", allReducePerfBinary, err)
		r.Details = map[string]any{"output": lastLines(string(output), 20)}
		return r
	}

	r.Details = map[string]any{
		"gpu_count":         gpuCount,
		"peak_busbw_gbytes": fmt.Sprintf("%.1f", res.PeakBusBWGBps),
		"wrong_count":       res.WrongCount,
		"out_of_bounds_ok":  res.OutOfBoundsOK,
		"pass_threshold":    c.passGBps,
		"warn_threshold":    c.warnGBps,
	}

	// Correctness first: any mismatched element or a non-OK out-of-bounds line
	// means the interconnect is corrupting data — unambiguously a failure.
	if res.WrongCount > 0 || !res.OutOfBoundsOK {
		r.Status = checks.StatusFail
		r.Message = fmt.Sprintf("NCCL all-reduce reported data errors (wrong=%d, out-of-bounds OK=%t)",
			res.WrongCount, res.OutOfBoundsOK)
		r.Remediation = "Data corruption over the GPU interconnect; check NVLink/NVSwitch health, ECC, and driver"
		return r
	}

	// Bandwidth: distinguishes real NVLink (hundreds of GB/s) from a PCIe
	// fallback or degraded fabric (tens of GB/s).
	switch {
	case res.PeakBusBWGBps >= c.passGBps:
		r.Status = checks.StatusPass
		r.Message = fmt.Sprintf("NVLink healthy: peak busbw %.1f GB/s across %d GPUs (threshold %.0f GB/s), no data errors",
			res.PeakBusBWGBps, gpuCount, c.passGBps)
	case res.PeakBusBWGBps >= c.warnGBps:
		r.Status = checks.StatusWarn
		r.Message = fmt.Sprintf("NVLink bus bandwidth low: peak busbw %.1f GB/s (below %.0f GB/s pass threshold)",
			res.PeakBusBWGBps, c.passGBps)
		r.Remediation = "Verify all NVLink lanes are active (nvidia-smi nvlink -s) and topology shows NV# links (nvidia-smi topo -m)"
	default:
		r.Status = checks.StatusFail
		r.Message = fmt.Sprintf("NVLink bus bandwidth very low: peak busbw %.1f GB/s (below %.0f GB/s warn threshold) — likely PCIe fallback or degraded NVLink",
			res.PeakBusBWGBps, c.warnGBps)
		r.Remediation = "GPUs are likely not communicating over NVLink; check nvidia-smi topo -m (expect NV#, not PHB/SYS), NVLink lane status, and fabric manager"
	}

	return r
}

// countGPUs returns the number of NVIDIA GPUs visible via nvidia-smi -L.
func countGPUs(ctx context.Context) (int, error) {
	output, err := exec.CommandContext(ctx, "nvidia-smi", "-L").Output()
	if err != nil {
		return 0, err
	}
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "GPU ") {
			count++
		}
	}
	return count, nil
}

// allReduceResult holds the health-relevant fields parsed from all_reduce_perf.
type allReduceResult struct {
	PeakBusBWGBps float64 // maximum busbw across all message sizes, in GB/s
	WrongCount    int     // total mismatched elements across all rows (#wrong columns)
	OutOfBoundsOK bool    // true if the "# Out of bounds values : N OK" line reports OK
}

// parseAllReduceOutput extracts peak bus bandwidth, total wrong count, and the
// out-of-bounds status from nccl-tests all_reduce_perf output.
//
// Data rows look like (columns are whitespace-separated):
//
//	size  count  type  redop  root  time  algbw  busbw  #wrong   time  algbw  busbw  #wrong
//
// i.e. two (busbw, #wrong) pairs per row (out-of-place and in-place). Lines
// starting with '#' are headers/summaries; the summary carries the out-of-bounds
// verdict: "# Out of bounds values : 0 OK".
func parseAllReduceOutput(output string) (allReduceResult, error) {
	var res allReduceResult
	sawData := false
	sawOOB := false

	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		if strings.HasPrefix(trimmed, "#") {
			if strings.Contains(trimmed, "Out of bounds values") {
				sawOOB = true
				res.OutOfBoundsOK = strings.HasSuffix(trimmed, "OK")
			}
			continue
		}

		fields := strings.Fields(trimmed)
		// A valid data row has both busbw/#wrong pairs; the two busbw columns are
		// at indices 7 and 11, the two #wrong columns at 8 and 12 (0-based).
		if len(fields) < 13 {
			continue
		}
		busbwOOP, err1 := strconv.ParseFloat(fields[7], 64)
		busbwIP, err2 := strconv.ParseFloat(fields[11], 64)
		wrongOOP, err3 := strconv.Atoi(fields[8])
		wrongIP, err4 := strconv.Atoi(fields[12])
		if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
			// Not a numeric data row (e.g. a header we don't recognize); skip.
			continue
		}

		sawData = true
		if busbwOOP > res.PeakBusBWGBps {
			res.PeakBusBWGBps = busbwOOP
		}
		if busbwIP > res.PeakBusBWGBps {
			res.PeakBusBWGBps = busbwIP
		}
		res.WrongCount += wrongOOP + wrongIP
	}

	if !sawData {
		return res, fmt.Errorf("no all_reduce_perf data rows found")
	}
	if !sawOOB {
		return res, fmt.Errorf("no out-of-bounds summary line found")
	}
	return res, nil
}

// lastLines returns the last n non-empty-trimmed lines of s, joined by newlines,
// for compact inclusion of tool output in a result's Details on failure.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}
