package config

import (
	"context"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// DetectPlatform auto-detects the cloud platform from node labels and provider IDs.
func DetectPlatform(ctx context.Context, client kubernetes.Interface) Platform {
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{Limit: 1})
	if err != nil || len(nodes.Items) == 0 {
		return PlatformUnknown
	}

	node := nodes.Items[0]
	labels := node.Labels
	providerID := node.Spec.ProviderID

	// AKS: provider ID starts with "azure://" or has Azure-specific labels
	if strings.HasPrefix(providerID, "azure://") {
		return PlatformAKS
	}
	if _, ok := labels["kubernetes.azure.com/cluster"]; ok {
		return PlatformAKS
	}

	// EKS: provider ID starts with "aws://" or has EKS-specific labels
	if strings.HasPrefix(providerID, "aws://") {
		return PlatformEKS
	}
	if _, ok := labels["eks.amazonaws.com/nodegroup"]; ok {
		return PlatformEKS
	}

	// CoreWeave: has CoreWeave-specific labels
	if _, ok := labels["coreweave.cloud/instance-type"]; ok {
		return PlatformCoreWeave
	}
	if strings.Contains(providerID, "coreweave") {
		return PlatformCoreWeave
	}

	// OCP: has OpenShift-specific labels
	if _, ok := labels["node.openshift.io/os_id"]; ok {
		return PlatformOCP
	}

	return PlatformUnknown
}
