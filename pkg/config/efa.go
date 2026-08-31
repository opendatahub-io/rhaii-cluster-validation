package config

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// EFAResourceName is the Kubernetes extended resource for AWS EFA devices.
//
// On EKS this is a device count: requesting N grants N EFA NICs in the pod.
// This differs from CoreWeave rdma/ib (and similar shared IB plugins) where
// "1" is a boolean flag meaning "attach host RDMA devices", not a literal count.
const EFAResourceName corev1.ResourceName = "vpc.amazonaws.com/efa"

// EFACountFromAllocatable returns the EFA device count in a node's allocatable
// resources. Returns 0 when EFA is not advertised.
func EFACountFromAllocatable(allocatable corev1.ResourceList) int64 {
	if qty, ok := allocatable[EFAResourceName]; ok && qty.Value() > 0 {
		return qty.Value()
	}
	return 0
}

// ResourceConfigHasEFA reports whether jobs resource config already specifies
// vpc.amazonaws.com/efa in requests or limits.
func ResourceConfigHasEFA(cfg ResourceConfig) bool {
	key := string(EFAResourceName)
	if _, ok := cfg.Requests[key]; ok {
		return true
	}
	_, ok := cfg.Limits[key]
	return ok
}

// AutoEFACount determines the EFA device count to request for a node.
// If a config override is set (via ConfigMap or platform config), use that;
// otherwise use the node's allocatable EFA count. Returns 0 if EFA is not available.
func AutoEFACount(nodeAllocatable corev1.ResourceList, configHasEFA bool, configValue int64) int64 {
	// Config override takes precedence
	if configHasEFA && configValue > 0 {
		return configValue
	}
	// Auto-detect from node allocatable
	return EFACountFromAllocatable(nodeAllocatable)
}

// parseEFACountQuantity parses a configured EFA quantity string. Returns a positive
// integral count, or 0 for malformed, fractional, or negative values.
func parseEFACountQuantity(s string) int64 {
	qty, err := resource.ParseQuantity(s)
	if err != nil || qty.Sign() < 0 {
		return 0
	}
	n, ok := qty.AsInt64()
	if !ok || n <= 0 {
		return 0
	}
	return n
}
