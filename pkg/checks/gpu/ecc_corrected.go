package gpu

import (
	"context"
	"encoding/csv"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
)

// ECCCorrectedCheck validates GPU ECC corrected memory errors.
// While corrected errors don't cause immediate data corruption, high counts
// indicate potential memory degradation that should be monitored.
type ECCCorrectedCheck struct {
	nodeName      string
	warnThreshold int
}

func NewECCCorrectedCheck(nodeName string, warnThreshold int) *ECCCorrectedCheck {
	return &ECCCorrectedCheck{
		nodeName:      nodeName,
		warnThreshold: warnThreshold,
	}
}

func (c *ECCCorrectedCheck) Name() string     { return "gpu_ecc_corrected_status" }
func (c *ECCCorrectedCheck) Category() string { return "gpu_hardware" }

func (c *ECCCorrectedCheck) Run(ctx context.Context) checks.Result {
	r := checks.Result{
		Node:     c.nodeName,
		Category: c.Category(),
		Name:     c.Name(),
	}

	output, err := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=index,ecc.errors.corrected.volatile.total",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		r.Status = checks.StatusSkip
		r.Message = fmt.Sprintf("nvidia-smi ECC corrected query failed: %v", err)
		return r
	}

	warnings, gpuCount, err := parseECCCorrectedOutput(string(output), c.warnThreshold)
	if err != nil {
		r.Status = checks.StatusFail
		r.Message = err.Error()
		return r
	}

	if gpuCount == 0 {
		r.Status = checks.StatusSkip
		r.Message = "No GPUs found"
		return r
	}

	if len(warnings) > 0 {
		r.Status = checks.StatusWarn
		r.Message = fmt.Sprintf("Corrected ECC errors detected: %s", strings.Join(warnings, "; "))
		r.Remediation = "Monitor GPU for increasing error rates, consider replacement if errors increase"
		r.Details = map[string]any{
			"warnings":       warnings,
			"warn_threshold": c.warnThreshold,
		}
		return r
	}

	r.Status = checks.StatusPass
	r.Message = fmt.Sprintf("No significant corrected ECC errors on %d GPU(s) (threshold: %d)",
		gpuCount, c.warnThreshold)
	r.Details = map[string]any{
		"warn_threshold": c.warnThreshold,
	}
	return r
}

// parseECCCorrectedOutput parses nvidia-smi CSV output for index,ecc.errors.corrected.volatile.total.
// Returns a list of warnings (empty if below threshold), the GPU count, and any parse error.
func parseECCCorrectedOutput(output string, warnThreshold int) (warnings []string, gpuCount int, err error) {
	reader := csv.NewReader(strings.NewReader(strings.TrimSpace(output)))
	records, csvErr := reader.ReadAll()
	if csvErr != nil {
		return nil, 0, fmt.Errorf("failed to parse nvidia-smi ECC corrected output")
	}

	for _, fields := range records {
		if len(fields) != 2 {
			return nil, 0, fmt.Errorf("invalid ECC corrected output: expected 2 fields, got %d", len(fields))
		}
		gpuIdx := strings.TrimSpace(fields[0])
		eccCorrected := strings.TrimSpace(fields[1])

		if eccCorrected != "N/A" && eccCorrected != "" {
			count, parseErr := strconv.Atoi(eccCorrected)
			if parseErr != nil {
				return nil, 0, fmt.Errorf("invalid ECC corrected count for GPU %s: %s", gpuIdx, eccCorrected)
			}
			if count >= warnThreshold {
				warnings = append(warnings, fmt.Sprintf("GPU %s: %d corrected errors (>= %d)", gpuIdx, count, warnThreshold))
			}
		}
	}

	return warnings, len(records), nil
}
