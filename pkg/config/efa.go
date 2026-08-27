package config

import corev1 "k8s.io/api/core/v1"

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
