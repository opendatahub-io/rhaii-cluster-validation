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

// PowerCheck validates GPU power consumption against thresholds.
type PowerCheck struct {
	nodeName          string
	warnPercentage    int
	errorPercentage   int
}

func NewPowerCheck(nodeName string, warnPercentage, errorPercentage int) *PowerCheck {
	// Ensure warn threshold is less than error threshold
	if warnPercentage >= errorPercentage {
		warnPercentage = errorPercentage - 1
		if warnPercentage < 0 {
			warnPercentage = 0
		}
	}
	return &PowerCheck{
		nodeName:        nodeName,
		warnPercentage:  warnPercentage,
		errorPercentage: errorPercentage,
	}
}

func (c *PowerCheck) Name() string     { return "gpu_power_consumption" }
func (c *PowerCheck) Category() string { return "gpu_hardware" }

func (c *PowerCheck) Run(ctx context.Context) checks.Result {
	r := checks.Result{
		Node:     c.nodeName,
		Category: c.Category(),
		Name:     c.Name(),
	}

	output, err := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=index,power.draw,power.limit",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		r.Status = checks.StatusSkip
		r.Message = fmt.Sprintf("nvidia-smi power query failed: %v", err)
		return r
	}

	powerStats, err := parsePowerOutput(string(output))
	if err != nil {
		r.Status = checks.StatusFail
		r.Message = err.Error()
		return r
	}

	if len(powerStats) == 0 {
		r.Status = checks.StatusSkip
		r.Message = "No GPUs found"
		return r
	}

	var warnings []string
	var errors []string
	maxPercentage := 0.0

	for _, ps := range powerStats {
		if ps.Percentage > maxPercentage {
			maxPercentage = ps.Percentage
		}
		if ps.Percentage >= float64(c.errorPercentage) {
			errors = append(errors, fmt.Sprintf("GPU %d: %.1fW/%.1fW (%.0f%% >= %d%%)",
				ps.Index, ps.Draw, ps.Limit, ps.Percentage, c.errorPercentage))
		} else if ps.Percentage >= float64(c.warnPercentage) {
			warnings = append(warnings, fmt.Sprintf("GPU %d: %.1fW/%.1fW (%.0f%% >= %d%%)",
				ps.Index, ps.Draw, ps.Limit, ps.Percentage, c.warnPercentage))
		}
	}

	r.Details = map[string]any{
		"power_stats":       powerStats,
		"max_percentage":    maxPercentage,
		"warn_percentage":   c.warnPercentage,
		"error_percentage":  c.errorPercentage,
	}

	if len(errors) > 0 {
		r.Status = checks.StatusFail
		r.Message = fmt.Sprintf("GPU power consumption critical: %s", strings.Join(errors, "; "))
		r.Remediation = "Reduce GPU workload, check power supply capacity, verify cooling"
		return r
	}

	if len(warnings) > 0 {
		r.Status = checks.StatusWarn
		r.Message = fmt.Sprintf("GPU power consumption elevated: %s", strings.Join(warnings, "; "))
		r.Remediation = "Monitor power consumption, consider workload optimization"
		return r
	}

	r.Status = checks.StatusPass
	r.Message = fmt.Sprintf("All %d GPU(s) power consumption normal (max: %.0f%%, thresholds: warn=%d%%, error=%d%%)",
		len(powerStats), maxPercentage, c.warnPercentage, c.errorPercentage)
	return r
}

// powerStat holds power consumption data for a single GPU.
type powerStat struct {
	Index      int     `json:"index"`
	Draw       float64 `json:"power_draw_watts"`
	Limit      float64 `json:"power_limit_watts"`
	Percentage float64 `json:"power_percentage"`
}

// parsePowerOutput parses nvidia-smi CSV output for index,power.draw,power.limit.
// Returns a slice of powerStat structs.
func parsePowerOutput(output string) ([]powerStat, error) {
	reader := csv.NewReader(strings.NewReader(strings.TrimSpace(output)))
	records, csvErr := reader.ReadAll()
	if csvErr != nil {
		return nil, fmt.Errorf("failed to parse nvidia-smi power output")
	}

	var stats []powerStat
	for _, fields := range records {
		if len(fields) != 3 {
			return nil, fmt.Errorf("invalid power output: expected 3 fields, got %d", len(fields))
		}
		gpuIdx, err := strconv.Atoi(strings.TrimSpace(fields[0]))
		if err != nil {
			return nil, fmt.Errorf("invalid GPU index: %s", fields[0])
		}
		powerDraw, err := strconv.ParseFloat(strings.TrimSpace(fields[1]), 64)
		if err != nil {
			return nil, fmt.Errorf("invalid power.draw for GPU %d: %s", gpuIdx, fields[1])
		}
		powerLimit, err := strconv.ParseFloat(strings.TrimSpace(fields[2]), 64)
		if err != nil {
			return nil, fmt.Errorf("invalid power.limit for GPU %d: %s", gpuIdx, fields[2])
		}

		percentage := 0.0
		if powerLimit > 0 {
			percentage = (powerDraw / powerLimit) * 100.0
		}

		stats = append(stats, powerStat{
			Index:      gpuIdx,
			Draw:       powerDraw,
			Limit:      powerLimit,
			Percentage: percentage,
		})
	}

	return stats, nil
}
