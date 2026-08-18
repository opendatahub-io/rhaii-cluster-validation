package config

import corev1 "k8s.io/api/core/v1"

// GPUNodeSelector maps a GPU vendor to the node label used to discover its nodes.
type GPUNodeSelector struct {
	Vendor   GPUVendor
	Selector string
}

// GPUNodeSelectors lists the label-based GPU node discovery selectors, in
// priority order.
var GPUNodeSelectors = []GPUNodeSelector{
	{Vendor: GPUVendorNVIDIA, Selector: "nvidia.com/gpu.present=true"},
	{Vendor: GPUVendorAMD, Selector: "amd.com/gpu.present=true"},
}

// GPUResourceNames are the known extended resource names for GPUs across vendors,
// used as a fallback when label-based discovery finds no nodes.
var GPUResourceNames = []corev1.ResourceName{
	"nvidia.com/gpu",
	"amd.com/gpu",
}

// GPUResourceForVendor returns the GPU extended resource name for a vendor.
func GPUResourceForVendor(vendor GPUVendor) corev1.ResourceName {
	switch vendor {
	case GPUVendorAMD:
		return "amd.com/gpu"
	default:
		return "nvidia.com/gpu"
	}
}

// GPUCountFromAllocatable returns the total GPU count found in a node's
// allocatable resources, checking all known vendor resource names.
func GPUCountFromAllocatable(allocatable corev1.ResourceList) int64 {
	for _, resName := range GPUResourceNames {
		if qty, ok := allocatable[resName]; ok && qty.Value() > 0 {
			return qty.Value()
		}
	}
	return 0
}

// GPUVendorFromResourceName infers the GPU vendor from an extended resource name.
func GPUVendorFromResourceName(name corev1.ResourceName) GPUVendor {
	switch name {
	case "nvidia.com/gpu":
		return GPUVendorNVIDIA
	case "amd.com/gpu":
		return GPUVendorAMD
	default:
		return GPUVendorUnknown
	}
}
