package config

import (
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

const (
	// MultusNetworksAnnotation is the pod annotation Multus reads for extra networks.
	MultusNetworksAnnotation = "k8s.v1.cni.cncf.io/networks"
	// NADResourceAnnotation names the device-plugin resource backing a NAD.
	NADResourceAnnotation   = "k8s.v1.cni.cncf.io/resourceName"
	sriovRDMAResourcePrefix = "openshift.io/"
)

// IsSRIOVRDMAResource is true for openshift.io/*rdma device-plugin resources.
func IsSRIOVRDMAResource(name corev1.ResourceName) bool {
	s := string(name)
	return strings.HasPrefix(s, sriovRDMAResourcePrefix) && strings.HasSuffix(s, "rdma")
}

// SRIOVRDMAResourcesFromAllocatable returns positive openshift.io/*rdma resources, sorted.
func SRIOVRDMAResourcesFromAllocatable(allocatable corev1.ResourceList) []string {
	var names []string
	for name, qty := range allocatable {
		if IsSRIOVRDMAResource(name) && qty.Value() > 0 {
			names = append(names, string(name))
		}
	}
	sort.Strings(names)
	return names
}

// ResourceConfigHasSRIOVRDMA is true when jobs config already pins an SR-IOV
// resource or a Multus attachment, so auto-detection must not override it.
func ResourceConfigHasSRIOVRDMA(cfg ResourceConfig) bool {
	if _, ok := cfg.Annotations[MultusNetworksAnnotation]; ok {
		return true
	}
	for _, set := range []map[string]string{cfg.Requests, cfg.Limits} {
		for key := range set {
			if IsSRIOVRDMAResource(corev1.ResourceName(key)) {
				return true
			}
		}
	}
	return false
}

// IntersectSRIOVRDMAResources returns resources advertised by every node.
// A partial rail is dropped so checker Jobs cannot stay Pending on nodes that lack it.
func IntersectSRIOVRDMAResources(perNode map[string][]string) []string {
	if len(perNode) == 0 {
		return nil
	}
	counts := make(map[string]int)
	for _, names := range perNode {
		for _, name := range names {
			counts[name]++
		}
	}
	var shared []string
	for name, n := range counts {
		if n == len(perNode) {
			shared = append(shared, name)
		}
	}
	sort.Strings(shared)
	return shared
}
