package config

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestIsSRIOVRDMAResource(t *testing.T) {
	tests := []struct {
		name string
		res  corev1.ResourceName
		want bool
	}{
		{"sriov rdma rail", "openshift.io/p4rdma", true},
		{"sriov rdma double digit rail", "openshift.io/p14rdma", true},
		{"sriov non-rdma pool", "openshift.io/p0_storage_sriov_nodepolicy", false},
		{"shared ib device plugin", "rdma/ib", false},
		{"nvidia roce", "nvidia.com/roce", false},
		{"efa", "vpc.amazonaws.com/efa", false},
		{"gpu", "nvidia.com/gpu", false},
		{"cpu", "cpu", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsSRIOVRDMAResource(tt.res); got != tt.want {
				t.Errorf("IsSRIOVRDMAResource(%q) = %v, want %v", tt.res, got, tt.want)
			}
		})
	}
}

func TestSRIOVRDMAResourcesFromAllocatable(t *testing.T) {
	allocatable := corev1.ResourceList{
		"openshift.io/p4rdma": resource.MustParse("8"),
		"openshift.io/p0rdma": resource.MustParse("8"),
		"openshift.io/p2rdma": resource.MustParse("0"), // exhausted, not usable
		"nvidia.com/gpu":      resource.MustParse("8"),
		"rdma/ib":             resource.MustParse("1"),
	}
	want := []string{"openshift.io/p0rdma", "openshift.io/p4rdma"}
	if got := SRIOVRDMAResourcesFromAllocatable(allocatable); !reflect.DeepEqual(got, want) {
		t.Errorf("SRIOVRDMAResourcesFromAllocatable() = %v, want %v", got, want)
	}
}

func TestIntersectSRIOVRDMAResourcesExcludesPartialRails(t *testing.T) {
	perNode := map[string][]string{
		"node-6-rail": {"openshift.io/p0rdma", "openshift.io/p2rdma", "openshift.io/p4rdma"},
		"node-8-rail": {"openshift.io/p0rdma", "openshift.io/p2rdma", "openshift.io/p4rdma", "openshift.io/p10rdma", "openshift.io/p12rdma"},
	}
	want := []string{"openshift.io/p0rdma", "openshift.io/p2rdma", "openshift.io/p4rdma"}
	if got := IntersectSRIOVRDMAResources(perNode); !reflect.DeepEqual(got, want) {
		t.Errorf("IntersectSRIOVRDMAResources() = %v, want %v", got, want)
	}
}

func TestIntersectSRIOVRDMAResourcesNodeWithoutSRIOV(t *testing.T) {
	perNode := map[string][]string{
		"node-a": {"openshift.io/p0rdma"},
		"node-b": nil,
	}
	if got := IntersectSRIOVRDMAResources(perNode); len(got) != 0 {
		t.Errorf("IntersectSRIOVRDMAResources() = %v, want empty when a node advertises none", got)
	}
}

func TestResourceConfigHasSRIOVRDMA(t *testing.T) {
	tests := []struct {
		name string
		cfg  ResourceConfig
		want bool
	}{
		{"empty", ResourceConfig{}, false},
		{"cpu only", ResourceConfig{Requests: map[string]string{"cpu": "500m"}}, false},
		{"shared ib", ResourceConfig{Requests: map[string]string{"rdma/ib": "1"}}, false},
		{"sriov in requests", ResourceConfig{Requests: map[string]string{"openshift.io/p4rdma": "1"}}, true},
		{"sriov in limits", ResourceConfig{Limits: map[string]string{"openshift.io/p4rdma": "1"}}, true},
		{"multus annotation", ResourceConfig{Annotations: map[string]string{MultusNetworksAnnotation: "roce-p4"}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResourceConfigHasSRIOVRDMA(tt.cfg); got != tt.want {
				t.Errorf("ResourceConfigHasSRIOVRDMA() = %v, want %v", got, tt.want)
			}
		})
	}
}
