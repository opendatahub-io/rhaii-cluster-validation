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

// TemperatureCheck validates GPU temperature against thresholds.
type TemperatureCheck struct {
	nodeName     string
	warnThreshold  int
	errorThreshold int
}

func NewTemperatureCheck(nodeName string, warnThreshold, errorThreshold int) *TemperatureCheck {
	return &TemperatureCheck{
		nodeName:       nodeName,
		warnThreshold:  warnThreshold,
		errorThreshold: errorThreshold,
	}
}

func (c *TemperatureCheck) Name() string     { return "gpu_temperature" }
func (c *TemperatureCheck) Category() string { return "gpu_hardware" }

func (c *TemperatureCheck) Run(ctx context.Context) checks.Result {
	r := checks.Result{
		Node:     c.nodeName,
		Category: c.Category(),
		Name:     c.Name(),
	}

	output, err := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=index,temperature.gpu",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		r.Status = checks.StatusSkip
		r.Message = fmt.Sprintf("nvidia-smi temperature query failed: %v", err)
		return r
	}

	temps, err := parseTemperatureOutput(string(output))
	if err != nil {
		r.Status = checks.StatusFail
		r.Message = err.Error()
		return r
	}

	var warnings []string
	var errors []string
	maxTemp := 0

	for gpuIdx, temp := range temps {
		if temp > maxTemp {
			maxTemp = temp
		}
		if temp >= c.errorThreshold {
			errors = append(errors, fmt.Sprintf("GPU %d: %d°C (>= %d°C)", gpuIdx, temp, c.errorThreshold))
		} else if temp >= c.warnThreshold {
			warnings = append(warnings, fmt.Sprintf("GPU %d: %d°C (>= %d°C)", gpuIdx, temp, c.warnThreshold))
		}
	}

	r.Details = map[string]any{
		"temperatures":      temps,
		"max_temperature":   maxTemp,
		"warn_threshold":    c.warnThreshold,
		"error_threshold":   c.errorThreshold,
	}

	if len(errors) > 0 {
		r.Status = checks.StatusFail
		r.Message = fmt.Sprintf("GPU temperature critical: %s", strings.Join(errors, "; "))
		r.Remediation = "Check cooling system, verify airflow, reduce GPU workload"
		return r
	}

	if len(warnings) > 0 {
		r.Status = checks.StatusWarn
		r.Message = fmt.Sprintf("GPU temperature elevated: %s", strings.Join(warnings, "; "))
		r.Remediation = "Monitor temperatures, check cooling system"
		return r
	}

	r.Status = checks.StatusPass
	r.Message = fmt.Sprintf("All %d GPU(s) within temperature range (max: %d°C, thresholds: warn=%d°C, error=%d°C)",
		len(temps), maxTemp, c.warnThreshold, c.errorThreshold)
	return r
}

// parseTemperatureOutput parses nvidia-smi CSV output for index,temperature.gpu.
// Returns a map of GPU index to temperature in Celsius.
func parseTemperatureOutput(output string) (map[int]int, error) {
	reader := csv.NewReader(strings.NewReader(strings.TrimSpace(output)))
	records, csvErr := reader.ReadAll()
	if csvErr != nil {
		return nil, fmt.Errorf("failed to parse nvidia-smi temperature output")
	}

	temps := make(map[int]int)
	for _, fields := range records {
		if len(fields) < 2 {
			continue
		}
		gpuIdx, err := strconv.Atoi(strings.TrimSpace(fields[0]))
		if err != nil {
			return nil, fmt.Errorf("invalid GPU index: %s", fields[0])
		}
		temp, err := strconv.Atoi(strings.TrimSpace(fields[1]))
		if err != nil {
			return nil, fmt.Errorf("invalid temperature value for GPU %d: %s", gpuIdx, fields[1])
		}
		temps[gpuIdx] = temp
	}

	return temps, nil
}
