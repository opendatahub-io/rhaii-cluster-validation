package config

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestGPUResourceForVendor(t *testing.T) {
	tests := []struct {
		vendor GPUVendor
		want   corev1.ResourceName
	}{
		{GPUVendorNVIDIA, "nvidia.com/gpu"},
		{GPUVendorAMD, "amd.com/gpu"},
		{GPUVendorUnknown, "nvidia.com/gpu"}, // defaults to NVIDIA
	}
	for _, tt := range tests {
		if got := GPUResourceForVendor(tt.vendor); got != tt.want {
			t.Errorf("GPUResourceForVendor(%q) = %q, want %q", tt.vendor, got, tt.want)
		}
	}
}

func TestGPUCountFromAllocatable(t *testing.T) {
	tests := []struct {
		name        string
		allocatable corev1.ResourceList
		want        int64
	}{
		{
			name:        "no gpu resources",
			allocatable: corev1.ResourceList{"cpu": resource.MustParse("4")},
			want:        0,
		},
		{
			name:        "nvidia gpus",
			allocatable: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("8")},
			want:        8,
		},
		{
			name:        "amd gpus",
			allocatable: corev1.ResourceList{"amd.com/gpu": resource.MustParse("4")},
			want:        4,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := GPUCountFromAllocatable(tt.allocatable); got != tt.want {
				t.Errorf("GPUCountFromAllocatable() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestGPUVendorFromResourceName(t *testing.T) {
	tests := []struct {
		name corev1.ResourceName
		want GPUVendor
	}{
		{"nvidia.com/gpu", GPUVendorNVIDIA},
		{"amd.com/gpu", GPUVendorAMD},
		{"cpu", GPUVendorUnknown},
	}
	for _, tt := range tests {
		if got := GPUVendorFromResourceName(tt.name); got != tt.want {
			t.Errorf("GPUVendorFromResourceName(%q) = %q, want %q", tt.name, got, tt.want)
		}
	}
}
