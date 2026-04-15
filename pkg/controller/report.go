package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"text/tabwriter"

	"golang.org/x/term"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks/rdma"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/jobrunner"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// jsonReport is the report structure used for both ConfigMap storage and JSON output.
type jsonReport struct {
	Platform      string                `json:"platform"`
	Timestamp     string                `json:"timestamp,omitempty"`
	ClusterChecks []checks.Result       `json:"cluster_checks,omitempty"`
	Nodes         []checks.NodeReport   `json:"nodes"`
	JobResults    []jobrunner.JobResult  `json:"job_results,omitempty"`
	Pingmesh      *rdma.PingMeshReport  `json:"pingmesh,omitempty"`
	Summary       map[string]int        `json:"summary"`
	Status        string                `json:"status"`
}

// countStatuses tallies pass/warn/fail/skip across all result sources.
func countStatuses(clusterResults []checks.Result, reports []checks.NodeReport, jobResults []jobrunner.JobResult, pingmesh *rdma.PingMeshReport) (pass, warn, fail, skip int) {
	for _, r := range clusterResults {
		switch r.Status {
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
	for _, report := range reports {
		for _, r := range report.Results {
			switch r.Status {
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
	}
	if pingmesh != nil {
		for _, s := range pingmesh.Summary {
			switch s.Status {
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
	}
	for _, jr := range jobResults {
		switch jr.Status {
		case checks.StatusPass:
			pass++
		case checks.StatusWarn:
			warn++
		case checks.StatusFail:
			fail++
		}
	}
	return
}

// readinessStatus returns the cluster readiness string based on fail/warn counts.
func readinessStatus(fail, warn int) string {
	if fail > 0 {
		return "NOT READY"
	}
	if warn > 0 {
		return "READY (with warnings)"
	}
	return "READY"
}

// --- terminal helpers ---

// isTTY reports whether stdout is a terminal.
var isTTY = sync.OnceValue(func() bool {
	return term.IsTerminal(int(os.Stdout.Fd()))
})

// termWidth returns the terminal width, defaulting to 120 if detection fails.
func termWidth() int {
	if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 0 {
		return w
	}
	return 120
}

// --- color helpers ---

func colorCode(s checks.Status) string {
	switch s {
	case checks.StatusPass:
		return "\033[32m"
	case checks.StatusFail:
		return "\033[31m"
	case checks.StatusWarn:
		return "\033[33m"
	default:
		return "\033[90m"
	}
}

// colorStatus returns the status string with ANSI color when outputting to a terminal.
func colorStatus(s checks.Status) string {
	if !isTTY() {
		return string(s)
	}
	return colorCode(s) + string(s) + "\033[0m"
}

// colorizeStatuses replaces status words in tabwriter output with colored versions.
// Applied after tabwriter.Flush so ANSI codes don't affect column alignment.
func colorizeStatuses(s string) string {
	if !isTTY() {
		return s
	}
	for _, st := range []checks.Status{checks.StatusPass, checks.StatusFail, checks.StatusWarn, checks.StatusSkip} {
		s = strings.ReplaceAll(s, "  "+string(st)+"  ", "  "+colorStatus(st)+"  ")
	}
	return s
}

// colorCount formats "N LABEL" with color when count > 0.
func colorCount(n int, label string, status checks.Status) string {
	if !isTTY() || n == 0 {
		return fmt.Sprintf("%d %s", n, label)
	}
	return fmt.Sprintf("%s%d %s\033[0m", colorCode(status), n, label)
}

// --- message wrapping ---

// wrapInColumn word-wraps msg to width, using tab continuations so tabwriter
// aligns wrapped lines under the same column.
func wrapInColumn(msg string, width int) string {
	if width < 20 || len(msg) <= width {
		return msg
	}
	var b strings.Builder
	for len(msg) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n\t\t\t\t")
		}
		if len(msg) <= width {
			b.WriteString(msg)
			break
		}
		cut := width
		if sp := strings.LastIndex(msg[:cut], " "); sp > 0 {
			cut = sp
		}
		b.WriteString(msg[:cut])
		msg = strings.TrimLeft(msg[cut:], " ")
	}
	return b.String()
}

// --- report printing ---

func (c *Controller) printReport(reports []checks.NodeReport, jobResults []jobrunner.JobResult) bool {
	fmt.Fprintln(c.output)
	fmt.Fprintln(c.output, "=== Validation Report ===")
	fmt.Fprintf(c.output, "Platform: %s\n", c.platform)

	// Print topology if available
	hasTopology := false
	for _, report := range reports {
		if topo := checks.ExtractTopology(report); topo != nil && len(topo.Pairs) > 0 {
			if !hasTopology {
				fmt.Fprintln(c.output)
				fmt.Fprintln(c.output, "GPU-NIC Topology:")
				hasTopology = true
			}
			var pairDescs []string
			for _, p := range topo.Pairs {
				pairDescs = append(pairDescs, fmt.Sprintf("GPU%d↔%s(NUMA:%d↔%d)", p.GPU.ID, p.NIC.Dev, p.GPU.NUMA, p.NIC.NUMA))
			}
			fmt.Fprintf(c.output, "  %s: %s\n", report.Node, strings.Join(pairDescs, ", "))
		}
	}

	fmt.Fprintln(c.output)

	// Compute max column widths to determine available space for message wrapping.
	const pad = 2 // tabwriter minpad
	groupW, checkW, nodeW, statusW := len("GROUP"), len("CHECK"), len("NODE"), len("STATUS")
	maxW := func(cur int, s string) int {
		if len(s) > cur {
			return len(s)
		}
		return cur
	}
	for _, r := range c.clusterResults {
		groupW = maxW(groupW, r.Category)
		checkW = maxW(checkW, r.Name)
		nodeW = maxW(nodeW, "(cluster)")
		statusW = maxW(statusW, string(r.Status))
	}
	for _, report := range reports {
		for _, r := range report.Results {
			groupW = maxW(groupW, r.Category)
			checkW = maxW(checkW, r.Name)
			nodeW = maxW(nodeW, r.Node)
			statusW = maxW(statusW, string(r.Status))
		}
	}
	if c.pingmeshReport != nil {
		groupW = maxW(groupW, "networking_rdma")
	}
	for _, jr := range jobResults {
		groupW = maxW(groupW, "bandwidth")
		checkW = maxW(checkW, jr.JobName)
		nodeW = maxW(nodeW, jr.Node)
		statusW = maxW(statusW, string(jr.Status))
	}
	prefixW := groupW + pad + checkW + pad + nodeW + pad + statusW + pad
	msgW := termWidth() - prefixW

	var buf strings.Builder
	tw := tabwriter.NewWriter(&buf, 0, 0, pad, ' ', 0)
	fmt.Fprintln(tw, "GROUP\tCHECK\tNODE\tSTATUS\tMESSAGE")

	for _, r := range c.clusterResults {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.Category, r.Name, "(cluster)", r.Status, wrapInColumn(r.Message, msgW))
		if r.Remediation != "" {
			fmt.Fprintf(tw, "\t\t\t\t%s\n", wrapInColumn("Fix: "+r.Remediation, msgW))
		}
	}

	for _, report := range reports {
		for _, r := range report.Results {
			node := r.Node
			if node == "" {
				node = "-"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.Category, r.Name, node, r.Status, wrapInColumn(r.Message, msgW))
			if r.Remediation != "" {
				fmt.Fprintf(tw, "\t\t\t\t%s\n", wrapInColumn("Fix: "+r.Remediation, msgW))
			}
		}
	}

	// Print pingmesh connectivity results (between per-node checks and bandwidth)
	if c.pingmeshReport != nil {
		for _, name := range []string{"rdma_conn_rail", "rdma_conn_xrail"} {
			if s, ok := c.pingmeshReport.Summary[name]; ok {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
					"networking_rdma", name, "(cluster)", s.Status, wrapInColumn(s.Message, msgW))
			}
		}
	}

	// Print job results (bandwidth tests)
	for _, jr := range jobResults {
		node := jr.Node
		if node == "" {
			node = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", "bandwidth", jr.JobName, node, jr.Status, wrapInColumn(jr.Message, msgW))
	}

	tw.Flush()
	fmt.Fprint(c.output, colorizeStatuses(buf.String()))

	pass, warn, fail, skip := countStatuses(c.clusterResults, reports, jobResults, c.pingmeshReport)

	fmt.Fprintln(c.output)
	fmt.Fprintf(c.output, "Summary: %s | %s | %s | %s\n",
		colorCount(pass, "PASS", checks.StatusPass),
		colorCount(warn, "WARN", checks.StatusWarn),
		colorCount(fail, "FAIL", checks.StatusFail),
		colorCount(skip, "SKIP", checks.StatusSkip))

	if fail > 0 {
		fmt.Fprintf(c.output, "Status:  %s\n", colorStatus(checks.StatusFail)+" - resolve FAIL items before deploying")
	} else {
		fmt.Fprintf(c.output, "Status:  %s\n", readinessStatus(fail, warn))
	}

	if c.reportStored {
		fmt.Fprintln(c.output)
		fmt.Fprintln(c.output, "Report:")
		fmt.Fprintf(c.output, "  kubectl get cm %s -n %s -o jsonpath='{.data.report\\.json}' | jq .\n", reportCMName, c.opts.Namespace)
	}
	fmt.Fprintln(c.output)

	return fail > 0
}

func (c *Controller) printJSONReport(reports []checks.NodeReport, jobResults []jobrunner.JobResult) bool {
	pass, warn, fail, skip := countStatuses(c.clusterResults, reports, jobResults, c.pingmeshReport)

	r := jsonReport{
		Platform:      string(c.platform),
		ClusterChecks: c.clusterResults,
		Nodes:         reports,
		JobResults:    jobResults,
		Pingmesh:      c.pingmeshReport,
		Summary:       map[string]int{"pass": pass, "warn": warn, "fail": fail, "skip": skip},
		Status:        readinessStatus(fail, warn),
	}

	data, _ := json.MarshalIndent(r, "", "  ")
	fmt.Fprintln(c.output, string(data))

	return fail > 0
}

// printDebugHelp lists actual pod/job names and useful debug commands.
func (c *Controller) printDebugHelp(ctx context.Context) {
	ns := c.opts.Namespace

	fmt.Fprintln(c.output, "")
	fmt.Fprintln(c.output, "=== DEBUG MODE ===")
	fmt.Fprintln(c.output, "Jobs kept alive for debugging.")
	fmt.Fprintln(c.output, "")

	// List all validation jobs (GPU check + net check + bandwidth)
	for _, selector := range []string{
		checkJobLabelKey + "=" + gpuCheckJobLabelValue,
		checkJobLabelKey + "=" + netCheckJobLabelValue,
		"app=rhaii-validate-job",
	} {
		jobs, err := c.client.BatchV1().Jobs(ns).List(ctx, metav1.ListOptions{
			LabelSelector: selector,
		})
		if err != nil || len(jobs.Items) == 0 {
			continue
		}
		fmt.Fprintf(c.output, "Jobs (%s):\n", selector)
		for _, j := range jobs.Items {
			fmt.Fprintf(c.output, "  kubectl logs -n %s -l job-name=%s\n", ns, j.Name)
		}
		fmt.Fprintln(c.output)
	}

	// List pods from check jobs (GPU + RDMA node)
	allCheckSelector := checkJobLabelKey + " in (" + gpuCheckJobLabelValue + "," + netCheckJobLabelValue + ")"
	pods, err := c.client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: allCheckSelector,
	})
	if err == nil && len(pods.Items) > 0 {
		fmt.Fprintln(c.output, "Check pods:")
		for _, pod := range pods.Items {
			fmt.Fprintf(c.output, "  %s (node: %s, status: %s)\n", pod.Name, pod.Spec.NodeName, pod.Status.Phase)
		}
		fmt.Fprintln(c.output, "")
		fmt.Fprintln(c.output, "Exec into pod:")
		for _, pod := range pods.Items {
			fmt.Fprintf(c.output, "  kubectl exec -it -n %s %s -- bash\n", ns, pod.Name)
		}
	}

	fmt.Fprintln(c.output, "")
	fmt.Fprintln(c.output, "Debug commands inside check pod:")
	fmt.Fprintln(c.output, "  nvidia-smi")
	fmt.Fprintln(c.output, "  chroot /host ibv_devices")
	fmt.Fprintln(c.output, "  chroot /host ibstat")
	fmt.Fprintln(c.output, "  ls /dev/nvidia*")
	fmt.Fprintln(c.output, "")
	fmt.Fprintf(c.output, "Cleanup: kubectl rhaii-validate clean\n")
}
