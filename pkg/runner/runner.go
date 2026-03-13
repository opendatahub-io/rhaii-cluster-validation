package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
)

// Runner executes all registered checks and outputs the report.
type Runner struct {
	nodeName string
	checks   []checks.Check
	output   io.Writer // JSON report goes here (stdout)
	log      io.Writer // Progress lines go here (stderr)
}

// New creates a new Runner for the given node.
func New(nodeName string, output io.Writer) *Runner {
	return &Runner{
		nodeName: nodeName,
		output:   output,
		log:      os.Stderr,
	}
}

// NewWithLog creates a Runner with explicit output and log writers.
func NewWithLog(nodeName string, output, log io.Writer) *Runner {
	return &Runner{
		nodeName: nodeName,
		output:   output,
		log:      log,
	}
}

// AddCheck registers a check to be executed.
func (r *Runner) AddCheck(c checks.Check) {
	r.checks = append(r.checks, c)
}

// Run executes all registered checks and writes the JSON report to output.
// Progress lines go to stderr, JSON report goes to stdout.
// Returns the completed report and any encoding error.
func (r *Runner) Run(ctx context.Context) (checks.NodeReport, error) {
	report := checks.NodeReport{
		Node:      r.nodeName,
		Timestamp: time.Now().UTC(),
	}

	for _, c := range r.checks {
		result := c.Run(ctx)
		report.Results = append(report.Results, result)

		// Progress to stderr (won't interfere with JSON on stdout)
		fmt.Fprintf(r.log, "[%s] %s/%s: %s\n",
			result.Status, result.Category, result.Name, result.Message)
	}

	// JSON report to stdout only
	encoder := json.NewEncoder(r.output)
	encoder.SetIndent("", "  ")
	return report, encoder.Encode(report)
}

// HasFailures returns true if any result in the report has StatusFail.
func HasFailures(report checks.NodeReport) bool {
	for _, r := range report.Results {
		if r.Status == checks.StatusFail {
			return true
		}
	}
	return false
}
