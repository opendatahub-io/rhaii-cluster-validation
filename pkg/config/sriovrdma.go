package config

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
)

const (
	// MultusNetworksAnnotation is the pod annotation Multus reads for extra networks.
	MultusNetworksAnnotation = "k8s.v1.cni.cncf.io/networks"
	// NADResourceAnnotation names the device-plugin resource backing a NAD.
	NADResourceAnnotation = "k8s.v1.cni.cncf.io/resourceName"
)

// ResourceConfigOwnsRDMAAttachment is true when jobs config sets a Multus attachment
// or any non-GPU extended resource; auto-detection then stays out so nothing is mixed.
func ResourceConfigOwnsRDMAAttachment(cfg ResourceConfig) bool {
	if _, ok := cfg.Annotations[MultusNetworksAnnotation]; ok {
		return true
	}
	for _, set := range []map[string]string{cfg.Requests, cfg.Limits} {
		for key := range set {
			if isRDMACandidateResource(corev1.ResourceName(key)) {
				return true
			}
		}
	}
	return false
}

// isRDMACandidateResource reports whether a jobs resource key could be an RDMA
// device: anything except core compute resources and GPUs.
func isRDMACandidateResource(name corev1.ResourceName) bool {
	switch name {
	case corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourceEphemeralStorage:
		return false
	}
	if strings.HasPrefix(string(name), corev1.ResourceHugePagesPrefix) {
		return false
	}
	for _, gpu := range GPUResourceNames {
		if name == gpu {
			return false
		}
	}
	return true
}
