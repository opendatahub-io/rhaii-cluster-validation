package config

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestDetectPlatform_AKS(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "aks-node-1",
			Labels: map[string]string{"kubernetes.azure.com/cluster": "my-cluster"},
		},
		Spec: corev1.NodeSpec{
			ProviderID: "azure:///subscriptions/xxx/resourceGroups/yyy",
		},
	})

	platform := DetectPlatform(context.Background(), client)
	if platform != PlatformAKS {
		t.Errorf("expected AKS, got %s", platform)
	}
}

func TestDetectPlatform_AKS_ByProviderID(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Spec:       corev1.NodeSpec{ProviderID: "azure:///subscriptions/xxx"},
	})

	platform := DetectPlatform(context.Background(), client)
	if platform != PlatformAKS {
		t.Errorf("expected AKS, got %s", platform)
	}
}

func TestDetectPlatform_EKS(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "eks-node-1",
			Labels: map[string]string{"eks.amazonaws.com/nodegroup": "my-nodegroup"},
		},
		Spec: corev1.NodeSpec{ProviderID: "aws:///us-east-1a/i-xxx"},
	})

	platform := DetectPlatform(context.Background(), client)
	if platform != PlatformEKS {
		t.Errorf("expected EKS, got %s", platform)
	}
}

func TestDetectPlatform_CoreWeave(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "cw-node-1",
			Labels: map[string]string{"coreweave.cloud/instance-type": "h100-80gb"},
		},
	})

	platform := DetectPlatform(context.Background(), client)
	if platform != PlatformCoreWeave {
		t.Errorf("expected CoreWeave, got %s", platform)
	}
}

func TestDetectPlatform_OCP(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "ocp-node-1",
			Labels: map[string]string{"node.openshift.io/os_id": "rhcos"},
		},
	})

	platform := DetectPlatform(context.Background(), client)
	if platform != PlatformOCP {
		t.Errorf("expected OCP, got %s", platform)
	}
}

func TestDetectPlatform_Unknown(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "generic-node"},
	})

	platform := DetectPlatform(context.Background(), client)
	if platform != PlatformUnknown {
		t.Errorf("expected Unknown, got %s", platform)
	}
}

func TestDetectPlatform_NoNodes(t *testing.T) {
	client := fake.NewSimpleClientset()

	platform := DetectPlatform(context.Background(), client)
	if platform != PlatformUnknown {
		t.Errorf("expected Unknown for empty cluster, got %s", platform)
	}
}
