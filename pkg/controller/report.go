package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/jobrunner"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/report"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// storeReport saves the JSON report to a ConfigMap so it persists after cleanup.
func (c *Controller) storeReport(ctx context.Context, reports []checks.NodeReport, jobResults []jobrunner.JobResult) error {
	r := report.Build(string(c.platform), time.Now().UTC().Format(time.RFC3339), c.clusterResults, reports, jobResults, c.pingmeshReport)

	// Merge with existing report: preserve fields this run didn't produce
	// (e.g. rdma-ping doesn't produce Nodes/JobResults, rdma-bandwidth doesn't produce Pingmesh)
	existing, getErr := c.client.CoreV1().ConfigMaps(c.opts.Namespace).Get(ctx, reportCMName, metav1.GetOptions{})
	if getErr == nil {
		if prev, ok := existing.Data["report.json"]; ok {
			var old report.Report
			if json.Unmarshal([]byte(prev), &old) == nil {
				r = report.MergePreserving(r, old)
			}
		}
	}

	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal report: %w", err)
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      reportCMName,
			Namespace: c.opts.Namespace,
			Labels:    map[string]string{"app": "rhaii-validator"},
		},
		Data: map[string]string{
			"report.json": string(data),
		},
	}

	// Update if exists, create if not
	if getErr == nil {
		existing.Data = cm.Data
		_, err = c.client.CoreV1().ConfigMaps(c.opts.Namespace).Update(ctx, existing, metav1.UpdateOptions{})
	} else if apierrors.IsNotFound(getErr) {
		_, err = c.client.CoreV1().ConfigMaps(c.opts.Namespace).Create(ctx, cm, metav1.CreateOptions{})
	} else {
		return getErr
	}

	if err != nil {
		return err
	}

	c.reportStored = true
	fmt.Fprintf(c.output, "  Report stored in ConfigMap %s/%s\n", c.opts.Namespace, reportCMName)
	return nil
}

func (c *Controller) printReport(reports []checks.NodeReport, jobResults []jobrunner.JobResult) bool {
	r := report.Build(string(c.platform), "", c.clusterResults, reports, jobResults, c.pingmeshReport)

	storedHint := ""
	if c.reportStored {
		storedHint = fmt.Sprintf("kubectl get cm %s -n %s -o jsonpath='{.data.report\\.json}' | jq .", reportCMName, c.opts.Namespace)
	}

	return report.FormatTable(c.output, string(c.platform), r, storedHint)
}

func (c *Controller) printJSONReport(reports []checks.NodeReport, jobResults []jobrunner.JobResult) bool {
	r := report.Build(string(c.platform), "", c.clusterResults, reports, jobResults, c.pingmeshReport)
	return report.FormatJSON(c.output, r)
}
