package controller

import (
	"fmt"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/config"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// applyAutoEFA injects vpc.amazonaws.com/efa for node on EKS unless jobs
// config already specifies EFA. Returns the count applied (0 if none).
// Used by rdma-node per-node check jobs.
func (c *Controller) applyAutoEFA(container *corev1.Container, nodeName string) int64 {
	if c.platform != config.PlatformEKS || config.ResourceConfigHasEFA(c.cfg.Jobs) {
		return 0
	}
	count, ok := c.efaCounts[nodeName]
	if !ok || count <= 0 {
		return 0
	}
	setContainerResource(container, config.EFAResourceName, count)
	return count
}

func setContainerResource(container *corev1.Container, res corev1.ResourceName, count int64) {
	if count <= 0 || res == "" {
		return
	}
	qty := resource.MustParse(fmt.Sprintf("%d", count))
	if container.Resources.Requests == nil {
		container.Resources.Requests = make(corev1.ResourceList)
	}
	if container.Resources.Limits == nil {
		container.Resources.Limits = make(corev1.ResourceList)
	}
	container.Resources.Requests[res] = qty
	container.Resources.Limits[res] = qty
}
