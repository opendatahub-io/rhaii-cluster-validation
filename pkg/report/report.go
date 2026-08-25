// Package report builds and formats the validation report shared by every
// check-mode: a table for interactive use, JSON for scripting, and the
// structure stored in the rhaii-validate-report ConfigMap. None of this
// package talks to Kubernetes — it only transforms result data already
// collected by the controller.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks/rdma"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/jobrunner"
)

// Report is the structure used for both ConfigMap storage and JSON output.
type Report struct {
	Platform      string                `json:"platform"`
	Timestamp     string                `json:"timestamp,omitempty"`
	ClusterChecks []checks.Result       `json:"cluster_checks,omitempty"`
	Nodes         []checks.NodeReport   `json:"nodes"`
	JobResults    []jobrunner.JobResult `json:"job_results,omitempty"`
	Pingmesh      *rdma.PingMeshReport  `json:"pingmesh,omitempty"`
	Summary       map[string]int        `json:"summary"`
	Status        string                `json:"status"`
}

// CountStatuses tallies pass/warn/fail/skip across all result sources.
func CountStatuses(clusterResults []checks.Result, reports []checks.NodeReport, jobResults []jobrunner.JobResult, pingmesh *rdma.PingMeshReport) (pass, warn, fail, skip int) {
	tally := func(s checks.Status) {
		switch s {
		case checks.StatusPass:
			pass++
		case checks.StatusWarn:
			warn++
		case checks.StatusFail:
			fail++
		case checks.StatusSkip:
			skip++
		}
	}
	for _, r := range clusterResults {
		tally(r.Status)
	}
	for _, report := range reports {
		for _, r := range report.Results {
			tally(r.Status)
		}
	}
	if pingmesh != nil {
		for _, s := range pingmesh.Summary {
			tally(s.Status)
		}
	}
	for _, jr := range jobResults {
		tally(jr.Status)
	}
	return
}

// ReadinessStatus returns the cluster readiness string based on fail/warn counts.
func ReadinessStatus(fail, warn int) string {
	if fail > 0 {
		return "NOT READY"
	}
	if warn > 0 {
		return "READY (with warnings)"
	}
	return "READY"
}

// Build assembles a Report from this run's results, computing Summary/Status.
func Build(platform, timestamp string, clusterResults []checks.Result, reports []checks.NodeReport, jobResults []jobrunner.JobResult, pingmesh *rdma.PingMeshReport) Report {
	pass, warn, fail, skip := CountStatuses(clusterResults, reports, jobResults, pingmesh)
	return Report{
		Platform:      platform,
		Timestamp:     timestamp,
		ClusterChecks: clusterResults,
		Nodes:         reports,
		JobResults:    jobResults,
		Pingmesh:      pingmesh,
		Summary:       map[string]int{"pass": pass, "warn": warn, "fail": fail, "skip": skip},
		Status:        ReadinessStatus(fail, warn),
	}
}

// MergePreserving fills in fields missing from current (this run) using
// previous (the prior stored report), then recomputes Summary/Status from the
// merged data. This lets runs that don't produce certain sections — e.g.
// rdma-ping doesn't produce Nodes/JobResults, rdma-bandwidth doesn't produce
// Pingmesh — keep showing results from earlier runs instead of dropping them.
func MergePreserving(current, previous Report) Report {
	if len(current.Nodes) == 0 && len(previous.Nodes) > 0 {
		current.Nodes = previous.Nodes
	}
	if len(current.JobResults) == 0 && len(previous.JobResults) > 0 {
		current.JobResults = previous.JobResults
	}
	if current.Pingmesh == nil && previous.Pingmesh != nil {
		current.Pingmesh = previous.Pingmesh
	}
	pass, warn, fail, skip := CountStatuses(current.ClusterChecks, current.Nodes, current.JobResults, current.Pingmesh)
	current.Summary = map[string]int{"pass": pass, "warn": warn, "fail": fail, "skip": skip}
	current.Status = ReadinessStatus(fail, warn)
	return current
}

// FormatTable writes the human-readable validation report to w and returns
// true if any result has FAIL status. storedHint, if non-empty, is printed
// as a follow-up command for retrieving the persisted report (e.g. a kubectl
// one-liner); it's omitted when empty.
func FormatTable(w io.Writer, platform string, r Report, storedHint string) bool {
	fmt.Fprintln(w)
	fmt.Fprintln(w, "=== Validation Report ===")
	fmt.Fprintf(w, "Platform: %s\n", platform)

	hasTopology := false
	for _, report := range r.Nodes {
		if topo := checks.ExtractTopology(report); topo != nil && len(topo.Pairs) > 0 {
			if !hasTopology {
				fmt.Fprintln(w)
				fmt.Fprintln(w, "GPU-NIC Topology:")
				hasTopology = true
			}
			var pairDescs []string
			for _, p := range topo.Pairs {
				pairDescs = append(pairDescs, fmt.Sprintf("GPU%d↔%s(NUMA:%d↔%d)", p.GPU.ID, p.NIC.Dev, p.GPU.NUMA, p.NIC.NUMA))
			}
			fmt.Fprintf(w, "  %s: %s\n", report.Node, strings.Join(pairDescs, ", "))
		}
	}

	fmt.Fprintln(w)

	fmt.Fprintf(w, "%-20s %-30s %-35s %-8s %s\n", "GROUP", "CHECK", "NODE", "STATUS", "MESSAGE")
	fmt.Fprintln(w, strings.Repeat("-", 130))

	for _, res := range r.ClusterChecks {
		fmt.Fprintf(w, "%-20s %-30s %-35s %-8s %s\n",
			res.Category, res.Name, "(cluster)", res.Status, res.Message)
		if res.Remediation != "" {
			fmt.Fprintf(w, "%-20s %-30s %-35s %-8s Fix: %s\n", "", "", "", "", res.Remediation)
		}
	}

	for _, report := range r.Nodes {
		for _, res := range report.Results {
			node := res.Node
			if node == "" {
				node = "-"
			}
			fmt.Fprintf(w, "%-20s %-30s %-35s %-8s %s\n",
				res.Category, res.Name, node, res.Status, res.Message)
			if res.Remediation != "" {
				fmt.Fprintf(w, "%-20s %-30s %-35s %-8s Fix: %s\n", "", "", "", "", res.Remediation)
			}
		}
	}

	// Pingmesh connectivity results (between per-node checks and bandwidth)
	if r.Pingmesh != nil {
		for _, name := range []string{"rdma_conn_rail", "rdma_conn_xrail"} {
			if s, ok := r.Pingmesh.Summary[name]; ok {
				fmt.Fprintf(w, "%-20s %-30s %-35s %-8s %s\n",
					"networking_rdma", name, "(cluster)", s.Status, s.Message)
			}
		}
	}

	for _, jr := range r.JobResults {
		node := jr.Node
		if node == "" {
			node = "-"
		}
		fmt.Fprintf(w, "%-20s %-30s %-35s %-8s %s\n",
			"bandwidth", jr.JobName, node, jr.Status, jr.Message)
	}

	fmt.Fprintln(w)
	fmt.Fprintf(w, "Summary: %d PASS | %d WARN | %d FAIL | %d SKIP\n", r.Summary["pass"], r.Summary["warn"], r.Summary["fail"], r.Summary["skip"])

	fail := r.Summary["fail"]
	warn := r.Summary["warn"]
	if fail > 0 {
		fmt.Fprintln(w, "Status:  NOT READY - resolve FAIL items before deploying")
	} else {
		fmt.Fprintf(w, "Status:  %s\n", ReadinessStatus(fail, warn))
	}

	if storedHint != "" {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Report:")
		fmt.Fprintf(w, "  %s\n", storedHint)
	}
	fmt.Fprintln(w)

	return fail > 0
}

// FormatJSON writes r as indented JSON to w and returns true if any result
// has FAIL status.
func FormatJSON(w io.Writer, r Report) bool {
	data, _ := json.MarshalIndent(r, "", "  ")
	fmt.Fprintln(w, string(data))
	return r.Summary["fail"] > 0
}
