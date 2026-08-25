package controller

import (
	"context"
	"fmt"
	"io"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks/crd"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks/operator"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/config"
)

// RunCRDChecks checks for required CRDs via the Kubernetes API (Tier 1).
func (c *Controller) RunCRDChecks(ctx context.Context) []checks.Result {
	checker := crd.NewChecker(c.client, nil, c.cfg.CRDs.MinAPIVersions, c.cfg.CRDs.MinReleaseVersions)
	return checker.Run(ctx)
}

// RunOperatorChecks checks that required operators have healthy pods (Tier 1).
func (c *Controller) RunOperatorChecks(ctx context.Context) []checks.Result {
	checker := operator.NewChecker(c.client, nil, c.cfg.Operators.Namespaces)
	return checker.Run(ctx)
}

// RunDeps runs Tier 1 dependency checks (CRDs + operator health) and prints the report.
// This is a lightweight path that doesn't create any cluster resources.
func (c *Controller) RunDeps(ctx context.Context) error {
	// Use stderr for progress so JSON mode stays machine-parseable on stdout
	log := c.output
	if c.opts.OutputFormat == "json" {
		log = io.Discard
	}

	fmt.Fprintln(log, "=== RHAII Dependency Checks ===")
	fmt.Fprintln(log)

	// Detect platform and load config so CRD min versions are available
	fmt.Fprintln(log, "Detecting platform...")
	c.platform = config.DetectPlatform(ctx, c.client)
	cfg, err := config.Load(c.platform, c.opts.ConfigFile)
	if err != nil {
		fmt.Fprintf(log, "  Warning: failed to load config override: %v, using platform defaults\n", err)
		cfg, _ = config.GetConfig(c.platform)
	}
	c.cfg = cfg
	fmt.Fprintf(log, "  Platform: %s\n", c.platform)

	fmt.Fprintln(log, "[CRD Checks] Checking required CRDs...")
	c.clusterResults = c.RunCRDChecks(ctx)
	for _, r := range c.clusterResults {
		fmt.Fprintf(log, "  [%s] %s: %s\n", r.Status, r.Name, r.Message)
	}
	fmt.Fprintln(log)

	fmt.Fprintln(log, "[Operator Checks] Checking operator health...")
	operatorResults := c.RunOperatorChecks(ctx)
	c.clusterResults = append(c.clusterResults, operatorResults...)
	for _, r := range operatorResults {
		fmt.Fprintf(log, "  [%s] %s: %s\n", r.Status, r.Name, r.Message)
	}
	fmt.Fprintln(log)

	var hasFailures bool
	if c.opts.OutputFormat == "json" {
		hasFailures = c.printJSONReport(nil, nil)
	} else {
		hasFailures = c.printReport(nil, nil)
	}

	if hasFailures {
		return fmt.Errorf("dependency check failed: one or more checks reported FAIL")
	}
	return nil
}
