package deploy

import (
	"bytes"
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestEnsureRBAC(t *testing.T) {
	client := fake.NewSimpleClientset()
	var out bytes.Buffer

	if err := EnsureRBAC(context.Background(), client, "test-ns", "", &out); err != nil {
		t.Fatalf("EnsureRBAC() error = %v", err)
	}

	sa, err := client.CoreV1().ServiceAccounts("test-ns").Get(context.Background(), ServiceAccountName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected ServiceAccount to be created: %v", err)
	}
	if sa.Namespace != "test-ns" {
		t.Errorf("ServiceAccount namespace = %q, want %q", sa.Namespace, "test-ns")
	}

	if _, err := client.RbacV1().ClusterRoles().Get(context.Background(), "rhaii-validator", metav1.GetOptions{}); err != nil {
		t.Errorf("expected ClusterRole to be created: %v", err)
	}

	crb, err := client.RbacV1().ClusterRoleBindings().Get(context.Background(), "rhaii-validator", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected ClusterRoleBinding to be created: %v", err)
	}
	if len(crb.Subjects) != 1 || crb.Subjects[0].Namespace != "test-ns" {
		t.Errorf("expected ClusterRoleBinding subject namespace rewritten to test-ns, got %+v", crb.Subjects)
	}

	// Idempotent: calling again should not error even though resources already exist.
	if err := EnsureRBAC(context.Background(), client, "test-ns", "", &out); err != nil {
		t.Fatalf("EnsureRBAC() second call error = %v", err)
	}
}

func TestEnsurePullSecret(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{Name: "rhaii-validator", Namespace: "test-ns"},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "my-pull-secret", Namespace: "test-ns"},
			Type:       corev1.SecretTypeDockerConfigJson,
		},
	)

	if err := EnsurePullSecret(context.Background(), client, "test-ns", "rhaii-validator", "my-pull-secret"); err != nil {
		t.Fatalf("EnsurePullSecret() error = %v", err)
	}

	sa, err := client.CoreV1().ServiceAccounts("test-ns").Get(context.Background(), "rhaii-validator", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get ServiceAccount: %v", err)
	}
	if len(sa.ImagePullSecrets) != 1 || sa.ImagePullSecrets[0].Name != "my-pull-secret" {
		t.Errorf("expected imagePullSecrets to contain my-pull-secret, got %+v", sa.ImagePullSecrets)
	}

	// Re-applying should not duplicate the entry.
	if err := EnsurePullSecret(context.Background(), client, "test-ns", "rhaii-validator", "my-pull-secret"); err != nil {
		t.Fatalf("EnsurePullSecret() second call error = %v", err)
	}
	sa, _ = client.CoreV1().ServiceAccounts("test-ns").Get(context.Background(), "rhaii-validator", metav1.GetOptions{})
	if len(sa.ImagePullSecrets) != 1 {
		t.Errorf("expected imagePullSecrets to stay deduplicated, got %+v", sa.ImagePullSecrets)
	}
}

func TestEnsurePullSecret_NotFound(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{Name: "rhaii-validator", Namespace: "test-ns"},
		},
	)

	err := EnsurePullSecret(context.Background(), client, "test-ns", "rhaii-validator", "missing-secret")
	if err == nil {
		t.Fatal("expected error for missing pull secret, got nil")
	}
}

func TestSCCBindingName(t *testing.T) {
	if got := SCCBindingName("rhaii-validation"); got != "rhaii-validator-scc-rhaii-validation" {
		t.Errorf("SCCBindingName() = %q, want %q", got, "rhaii-validator-scc-rhaii-validation")
	}
}

func TestEnsureOpenShiftSCC(t *testing.T) {
	client := fake.NewSimpleClientset()

	if err := EnsureOpenShiftSCC(context.Background(), client, "test-ns"); err != nil {
		t.Fatalf("EnsureOpenShiftSCC() error = %v", err)
	}

	crb, err := client.RbacV1().ClusterRoleBindings().Get(context.Background(), SCCBindingName("test-ns"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected SCC ClusterRoleBinding to be created: %v", err)
	}
	if crb.RoleRef.Name != "system:openshift:scc:privileged" {
		t.Errorf("RoleRef.Name = %q, want %q", crb.RoleRef.Name, "system:openshift:scc:privileged")
	}

	// Idempotent.
	if err := EnsureOpenShiftSCC(context.Background(), client, "test-ns"); err != nil {
		t.Fatalf("EnsureOpenShiftSCC() second call error = %v", err)
	}
}
