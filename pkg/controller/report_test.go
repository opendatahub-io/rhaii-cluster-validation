package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/report"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func sampleNodeReports(status checks.Status) []checks.NodeReport {
	return []checks.NodeReport{
		{
			Node: "node-1",
			Results: []checks.Result{
				{Node: "node-1", Category: "gpu_hardware", Name: "gpu_driver_version", Status: status, Message: "test"},
			},
		},
	}
}

func TestStoreReport_CreatesConfigMap(t *testing.T) {
	c, _ := newTestController(nil)
	c.platform = "AKS"

	if err := c.storeReport(context.Background(), sampleNodeReports(checks.StatusPass), nil); err != nil {
		t.Fatalf("storeReport() error = %v", err)
	}
	if !c.reportStored {
		t.Error("expected reportStored to be true after a successful store")
	}

	cm, err := c.client.CoreV1().ConfigMaps(c.opts.Namespace).Get(context.Background(), reportCMName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected report ConfigMap to be created: %v", err)
	}
	var r report.Report
	if err := json.Unmarshal([]byte(cm.Data["report.json"]), &r); err != nil {
		t.Fatalf("failed to unmarshal stored report: %v", err)
	}
	if r.Platform != "AKS" {
		t.Errorf("Platform = %q, want AKS", r.Platform)
	}
	if len(r.Nodes) != 1 {
		t.Fatalf("expected 1 node report, got %d", len(r.Nodes))
	}
}

func TestStoreReport_MergesWithExisting(t *testing.T) {
	c, _ := newTestController(nil)
	c.platform = "AKS"

	// First run: rdma-node produces node reports.
	if err := c.storeReport(context.Background(), sampleNodeReports(checks.StatusPass), nil); err != nil {
		t.Fatalf("first storeReport() error = %v", err)
	}

	// Second run: rdma-ping doesn't produce Nodes, so the previous Nodes
	// should be preserved by MergePreserving.
	if err := c.storeReport(context.Background(), nil, nil); err != nil {
		t.Fatalf("second storeReport() error = %v", err)
	}

	cm, err := c.client.CoreV1().ConfigMaps(c.opts.Namespace).Get(context.Background(), reportCMName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get report ConfigMap: %v", err)
	}
	var r report.Report
	if err := json.Unmarshal([]byte(cm.Data["report.json"]), &r); err != nil {
		t.Fatalf("failed to unmarshal stored report: %v", err)
	}
	if len(r.Nodes) != 1 {
		t.Errorf("expected previous node reports to be preserved, got %d nodes", len(r.Nodes))
	}
}

func TestPrintReport_NoFailures(t *testing.T) {
	c, buf := newTestController(nil)
	c.platform = "AKS"

	hasFailures := c.printReport(sampleNodeReports(checks.StatusPass), nil)
	if hasFailures {
		t.Error("expected hasFailures = false for all-PASS report")
	}
	if !strings.Contains(buf.String(), "Validation Report") {
		t.Errorf("expected table header in output, got: %s", buf.String())
	}
}

func TestPrintReport_WithFailures(t *testing.T) {
	c, _ := newTestController(nil)
	c.platform = "AKS"

	hasFailures := c.printReport(sampleNodeReports(checks.StatusFail), nil)
	if !hasFailures {
		t.Error("expected hasFailures = true when a result is FAIL")
	}
}

func TestPrintJSONReport(t *testing.T) {
	c, buf := newTestController(nil)
	c.platform = "AKS"

	hasFailures := c.printJSONReport(sampleNodeReports(checks.StatusWarn), nil)
	if hasFailures {
		t.Error("expected hasFailures = false for WARN-only report")
	}

	var r report.Report
	if err := json.Unmarshal(buf.Bytes(), &r); err != nil {
		t.Fatalf("expected valid JSON output, got error: %v (output: %s)", err, buf.String())
	}
	if r.Platform != "AKS" {
		t.Errorf("Platform = %q, want AKS", r.Platform)
	}
}
