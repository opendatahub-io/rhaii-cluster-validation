package controller

import (
	"context"
	"fmt"

	"github.com/opendatahub-io/rhaii-cluster-validation/deploy"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// printDebugHelp lists actual pod/job names and useful debug commands.
func (c *Controller) printDebugHelp(ctx context.Context) {
	ns := c.opts.Namespace

	fmt.Fprintln(c.output, "")
	fmt.Fprintln(c.output, "=== DEBUG MODE ===")
	fmt.Fprintln(c.output, "Jobs kept alive for debugging.")
	fmt.Fprintln(c.output, "")

	// List all validation jobs (GPU check + net check + BW probe + bandwidth)
	for _, selector := range []string{
		checkJobLabelKey + "=" + gpuCheckJobLabelValue,
		checkJobLabelKey + "=" + netCheckJobLabelValue,
		checkJobLabelKey + "=" + bwProbeLabelValue,
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

	// List pods from check jobs (GPU + RDMA node + BW probe)
	allCheckSelector := checkJobLabelKey + " in (" + gpuCheckJobLabelValue + "," + netCheckJobLabelValue + "," + bwProbeLabelValue + ")"
	pods, err := c.client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: allCheckSelector,
	})
	if err == nil && len(pods.Items) > 0 {
		fmt.Fprintln(c.output, "Pods:")
		for _, pod := range pods.Items {
			fmt.Fprintf(c.output, "  %s (node: %s, status: %s)\n", pod.Name, pod.Spec.NodeName, pod.Status.Phase)
		}
		fmt.Fprintln(c.output)
		fmt.Fprintln(c.output, "View logs:")
		for _, pod := range pods.Items {
			fmt.Fprintf(c.output, "  kubectl logs -n %s %s\n", ns, pod.Name)
		}
	}

	fmt.Fprintln(c.output, "")
	fmt.Fprintf(c.output, "Cleanup: kubectl rhaii-validate clean\n")
}

// printDebugCleanupHint prints the exact commands to manually remove cluster-scoped
// RBAC resources that were left behind by --debug mode.
func (c *Controller) printDebugCleanupHint() {
	ns := c.opts.Namespace
	fmt.Fprintln(c.output, "")
	fmt.Fprintln(c.output, "NOTE: --debug mode skips automatic cleanup. Cluster-scoped RBAC resources remain.")
	fmt.Fprintln(c.output, "To remove them manually:")
	fmt.Fprintf(c.output, "  kubectl delete clusterrolebinding rhaii-validator %s\n", deploy.SCCBindingName(c.opts.Namespace))
	fmt.Fprintf(c.output, "  kubectl delete clusterrole rhaii-validator\n")
	fmt.Fprintf(c.output, "  kubectl delete serviceaccount -n %s rhaii-validator\n", ns)
	fmt.Fprintln(c.output, "Or run: kubectl rhaii-validate clean")
}
