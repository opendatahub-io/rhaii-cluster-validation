package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/opendatahub-io/rhaii-cluster-validation/deploy"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// cleanupGpuCheckJobs deletes all GPU check jobs and waits for them to be fully removed.
func (c *Controller) cleanupGpuCheckJobs(ctx context.Context) bool {
	return c.deleteJobsBySelector(ctx, checkJobLabelKey+"="+gpuCheckJobLabelValue)
}

// cleanupNetCheckJobs deletes all RDMA node check jobs and waits for them to be fully removed.
func (c *Controller) cleanupNetCheckJobs(ctx context.Context) bool {
	return c.deleteJobsBySelector(ctx, checkJobLabelKey+"="+netCheckJobLabelValue)
}

// cleanupLoopbackBWProbeJobs deletes all BW probe jobs and waits for removal.
func (c *Controller) cleanupLoopbackBWProbeJobs(ctx context.Context) bool {
	return c.deleteJobsBySelector(ctx, checkJobLabelKey+"="+bwProbeLabelValue)
}

// cleanupBandwidthJobs deletes all bandwidth jobs and waits for them to be fully removed.
func (c *Controller) cleanupBandwidthJobs(ctx context.Context) bool {
	return c.deleteJobsBySelector(ctx, "app=rhaii-validate-job")
}

// cleanupPingMeshJobs deletes pingmesh jobs only. The failures ConfigMap is
// managed by runPingMesh: updated on failure, deleted on full success.
func (c *Controller) cleanupPingMeshJobs(ctx context.Context) bool {
	return c.deleteJobsBySelector(ctx, "rhaii-job-type=pingmesh")
}

// deleteJobsBySelector deletes jobs matching a label selector and waits for pod termination.
// Uses context.Background() so cleanup completes even after signal interruption.
// Uses Background propagation (non-blocking Job GC) and polls for pod termination
// which is the real gate for freeing GPU/RDMA device resources.
// Returns true if all pods terminated within the timeout, false if some remain.
func (c *Controller) deleteJobsBySelector(_ context.Context, selector string) bool {
	bgCtx := context.Background()
	propagation := metav1.DeletePropagationBackground
	jobs, err := c.client.BatchV1().Jobs(c.opts.Namespace).List(bgCtx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil || len(jobs.Items) == 0 {
		return true
	}

	for _, j := range jobs.Items {
		if delErr := c.client.BatchV1().Jobs(c.opts.Namespace).Delete(bgCtx, j.Name, metav1.DeleteOptions{
			PropagationPolicy: &propagation,
		}); delErr != nil && !apierrors.IsNotFound(delErr) {
			fmt.Fprintf(c.output, "  Warning: failed to delete job %s: %v\n", j.Name, delErr)
		}
	}
	fmt.Fprintf(c.output, "  Deleting %d leftover job(s) (%s)...\n", len(jobs.Items), selector)

	for i := 0; i < 30; i++ {
		pods, err := c.client.CoreV1().Pods(c.opts.Namespace).List(bgCtx, metav1.ListOptions{
			LabelSelector: selector,
		})
		if err != nil || len(pods.Items) == 0 {
			return true
		}
		time.Sleep(1 * time.Second)
	}
	fmt.Fprintf(c.output, "  Warning: some pods still terminating for %s\n", selector)
	return false
}

// cleanupAll removes check jobs, bandwidth jobs, and RBAC resources.
// ConfigMap is preserved so users can edit and rerun without losing customizations.
// Uses context.Background() (via deleteJobsBySelector) so cleanup completes after ^C.
func (c *Controller) cleanupAll(ctx context.Context) error {
	c.cleanupGpuCheckJobs(ctx)
	c.cleanupNetCheckJobs(ctx)
	c.cleanupLoopbackBWProbeJobs(ctx)
	c.cleanupBandwidthJobs(ctx)
	c.cleanupPingMeshJobs(ctx)

	bgCtx := context.Background()
	for _, del := range []func() error{
		func() error {
			return c.client.CoreV1().ServiceAccounts(c.opts.Namespace).Delete(bgCtx, "rhaii-validator", metav1.DeleteOptions{})
		},
		func() error {
			return c.client.RbacV1().ClusterRoleBindings().Delete(bgCtx, "rhaii-validator", metav1.DeleteOptions{})
		},
		func() error {
			return c.client.RbacV1().ClusterRoleBindings().Delete(bgCtx, deploy.SCCBindingName(c.opts.Namespace), metav1.DeleteOptions{})
		},
		func() error {
			return c.client.RbacV1().ClusterRoles().Delete(bgCtx, "rhaii-validator", metav1.DeleteOptions{})
		},
	} {
		if err := del(); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}
