package deploy

import (
	"context"
	"fmt"
	"io"
	"strings"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	k8syaml "sigs.k8s.io/yaml"
)

// ServiceAccountName is the name of the ServiceAccount used by validation
// Jobs and, on OpenShift, bound to the privileged SCC.
const ServiceAccountName = "rhaii-validator"

// EnsureRBAC applies the embedded RBAC manifest (ServiceAccount, ClusterRole,
// ClusterRoleBinding) into namespace, attaching pullSecret to the
// ServiceAccount's imagePullSecrets if provided. Resources that already exist
// are left untouched (idempotent).
func EnsureRBAC(ctx context.Context, client kubernetes.Interface, namespace, pullSecret string, output io.Writer) error {
	docs := splitYAMLDocuments(RBACYAML)

	for _, doc := range docs {
		if len(doc) == 0 {
			continue
		}

		// Peek at the kind to decide how to unmarshal
		var meta struct {
			Kind string `json:"kind"`
		}
		if err := k8syaml.Unmarshal(doc, &meta); err != nil {
			continue
		}

		switch meta.Kind {
		case "Namespace":
			// Skip — the namespace is created separately using the caller's --namespace flag.
			continue

		case "ServiceAccount":
			var sa corev1.ServiceAccount
			if err := k8syaml.Unmarshal(doc, &sa); err != nil {
				return fmt.Errorf("failed to parse ServiceAccount: %w", err)
			}
			sa.Namespace = namespace
			_, err := client.CoreV1().ServiceAccounts(namespace).Create(ctx, &sa, metav1.CreateOptions{})
			if err != nil && !apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("failed to create ServiceAccount: %w", err)
			}
			if err := EnsurePullSecret(ctx, client, namespace, sa.Name, pullSecret); err != nil {
				return err
			}

		case "ClusterRole":
			var cr rbacv1.ClusterRole
			if err := k8syaml.Unmarshal(doc, &cr); err != nil {
				return fmt.Errorf("failed to parse ClusterRole: %w", err)
			}
			_, err := client.RbacV1().ClusterRoles().Create(ctx, &cr, metav1.CreateOptions{})
			if err != nil && !apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("failed to create ClusterRole: %w", err)
			}

		case "ClusterRoleBinding":
			var crb rbacv1.ClusterRoleBinding
			if err := k8syaml.Unmarshal(doc, &crb); err != nil {
				return fmt.Errorf("failed to parse ClusterRoleBinding: %w", err)
			}
			// Update the subject namespace to match the caller's --namespace
			for i := range crb.Subjects {
				if crb.Subjects[i].Kind == "ServiceAccount" {
					crb.Subjects[i].Namespace = namespace
				}
			}
			_, err := client.RbacV1().ClusterRoleBindings().Create(ctx, &crb, metav1.CreateOptions{})
			if err != nil && !apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("failed to create ClusterRoleBinding: %w", err)
			}

		default:
			fmt.Fprintf(output, "  Warning: skipping unknown RBAC resource kind %q\n", meta.Kind)
		}
	}

	return nil
}

// EnsurePullSecret ensures pullSecret (if non-empty) is attached to the named
// ServiceAccount's imagePullSecrets, preserving any secrets already present.
func EnsurePullSecret(ctx context.Context, client kubernetes.Interface, namespace, saName, pullSecret string) error {
	sa, err := client.CoreV1().ServiceAccounts(namespace).Get(ctx, saName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get ServiceAccount %s: %w", saName, err)
	}

	if pullSecret == "" {
		return nil
	}

	secret, err := client.CoreV1().Secrets(namespace).Get(ctx, pullSecret, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("pull secret %q not found in namespace %s — create it first:\n  kubectl create secret docker-registry %s -n %s --from-file=.dockerconfigjson=<path>",
				pullSecret, namespace, pullSecret, namespace)
		}
		return fmt.Errorf("failed to get pull secret %q: %w", pullSecret, err)
	}
	if secret.Type != corev1.SecretTypeDockerConfigJson {
		return fmt.Errorf("secret %q has type %q, expected %q", pullSecret, secret.Type, corev1.SecretTypeDockerConfigJson)
	}

	for _, ref := range sa.ImagePullSecrets {
		if ref.Name == pullSecret {
			return nil
		}
	}

	sa.ImagePullSecrets = append(sa.ImagePullSecrets, corev1.LocalObjectReference{Name: pullSecret})
	_, err = client.CoreV1().ServiceAccounts(namespace).Update(ctx, sa, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update ServiceAccount %s with pull secret: %w", saName, err)
	}
	return nil
}

// SCCBindingName returns the name of the ClusterRoleBinding used to grant the
// privileged SCC to the validator ServiceAccount on OpenShift. It is
// namespace-scoped so multiple validation runs in different namespaces don't collide.
func SCCBindingName(namespace string) string {
	return "rhaii-validator-scc-" + namespace
}

// EnsureOpenShiftSCC grants the privileged SCC to the validator ServiceAccount
// in namespace. Check Jobs need privileged access for host sysfs visibility
// (PCI topology, RDMA device discovery via /sys/class/infiniband).
func EnsureOpenShiftSCC(ctx context.Context, client kubernetes.Interface, namespace string) error {
	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: SCCBindingName(namespace),
		},
		Subjects: []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      ServiceAccountName,
			Namespace: namespace,
		}},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     "system:openshift:scc:privileged",
		},
	}

	_, err := client.RbacV1().ClusterRoleBindings().Create(ctx, crb, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// splitYAMLDocuments splits a multi-document YAML byte slice on "---" separators.
func splitYAMLDocuments(data []byte) [][]byte {
	var docs [][]byte
	for _, part := range strings.Split(string(data), "\n---") {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			docs = append(docs, []byte(trimmed))
		}
	}
	return docs
}
