package config

import "testing"

func TestResourceConfigOwnsRDMAAttachment(t *testing.T) {
	tests := []struct {
		name string
		cfg  ResourceConfig
		want bool
	}{
		{"platform default cpu/memory only", ResourceConfig{Requests: map[string]string{"cpu": "500m", "memory": "512Mi"}}, false},
		{"GPU and hugepages are not RDMA choices", ResourceConfig{Limits: map[string]string{"nvidia.com/gpu": "1", "hugepages-2Mi": "1Gi", "ephemeral-storage": "1Gi"}}, false},
		{"Multus annotation", ResourceConfig{Annotations: map[string]string{MultusNetworksAnnotation: "ns/roce-p2"}}, true},
		{"SR-IOV resource", ResourceConfig{Requests: map[string]string{"openshift.io/p2rdma": "1"}}, true},
		{"shared-plugin resource", ResourceConfig{Requests: map[string]string{"rdma/ib": "1"}}, true},
		{"resource set only in limits", ResourceConfig{Limits: map[string]string{"nvidia.com/roce": "1"}}, true},
		{"unrelated annotation", ResourceConfig{Annotations: map[string]string{"example.com/team": "a"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResourceConfigOwnsRDMAAttachment(tt.cfg); got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}
