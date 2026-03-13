package gpu

import (
	"context"
	"encoding/csv"
	"fmt"
	"os/exec"
	"strings"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
)

// ECCCheck validates GPU ECC memory error status.
type ECCCheck struct {
	nodeName string
}

func NewECCCheck(nodeName string) *ECCCheck {
	return &ECCCheck{nodeName: nodeName}
}

func (c *ECCCheck) Name() string     { return "gpu_ecc_status" }
func (c *ECCCheck) Category() string { return "gpu_hardware" }

func (c *ECCCheck) Run(ctx context.Context) checks.Result {
	r := checks.Result{
		Node:     c.nodeName,
		Category: c.Category(),
		Name:     c.Name(),
	}

	output, err := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=index,ecc.errors.uncorrected.volatile.total",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		r.Status = checks.StatusSkip
		r.Message = fmt.Sprintf("nvidia-smi ECC query failed: %v", err)
		return r
	}

	errorsFound, gpuCount, err := parseECCOutput(string(output))
	if err != nil {
		r.Status = checks.StatusFail
		r.Message = err.Error()
		return r
	}

	if len(errorsFound) > 0 {
		r.Status = checks.StatusFail
		r.Message = fmt.Sprintf("Uncorrectable ECC errors found: %s", strings.Join(errorsFound, "; "))
		r.Remediation = "Replace GPU or contact cloud provider"
		r.Details = map[string]any{"errors": errorsFound}
		return r
	}

	r.Status = checks.StatusPass
	r.Message = fmt.Sprintf("No uncorrectable ECC errors on %d GPU(s)", gpuCount)
	return r
}

// parseECCOutput parses nvidia-smi CSV output for index,ecc.errors.uncorrected.volatile.total.
// Returns a list of error descriptions (empty if no errors), the GPU count, and any parse error.
func parseECCOutput(output string) (errorsFound []string, gpuCount int, err error) {
	reader := csv.NewReader(strings.NewReader(strings.TrimSpace(output)))
	records, csvErr := reader.ReadAll()
	if csvErr != nil {
		return nil, 0, fmt.Errorf("failed to parse nvidia-smi ECC output")
	}

	for _, fields := range records {
		if len(fields) < 2 {
			continue
		}
		gpuIdx := strings.TrimSpace(fields[0])
		eccErrors := strings.TrimSpace(fields[1])
		if eccErrors != "0" && eccErrors != "N/A" && eccErrors != "" {
			errorsFound = append(errorsFound, fmt.Sprintf("GPU %s: %s uncorrectable errors", gpuIdx, eccErrors))
		}
	}

	return errorsFound, len(records), nil
}
