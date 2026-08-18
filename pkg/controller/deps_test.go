package controller

import (
	"context"
	"testing"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/config"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// Note: RunCRDChecks (and therefore RunDeps, which calls it) is not exercised
// here with the fake clientset: it queries the discovery RESTClient directly
// (c.client.Discovery().RESTClient()...), which client-go's fake Discovery
// implementation returns as nil, panicking on any call. CRD-checking logic
// itself is covered by pkg/checks/crd's own unit tests.

func TestRunOperatorChecks_NoOverridesUsesDefaultOperatorsAndFails(t *testing.T) {
	// With no platform-config overrides, the checker falls back to the
	// built-in RequiredOperators list, and since none of those namespaces
	// exist on the fake cluster, every operator reports a non-passing status:
	// FAIL for required operators, WARN for optional ones (e.g. lws). Assert
	// "not PASS" rather than "FAIL" so this stays valid as operators are marked
	// optional.
	c, _ := newTestController(nil)
	c.cfg = config.PlatformConfig{}

	results := c.RunOperatorChecks(context.Background())
	if len(results) == 0 {
		t.Fatal("expected default operator checks to run")
	}
	for _, r := range results {
		if r.Status != checks.StatusFail && r.Status != checks.StatusWarn {
			t.Errorf("expected FAIL or WARN for operator %s on an empty cluster, got %s", r.Name, r.Status)
		}
	}
}

func TestRunOperatorChecks_ReportsMissingOperatorPods(t *testing.T) {
	c, _ := newTestController(nil)
	c.cfg = config.PlatformConfig{
		Operators: config.OperatorConfig{
			Namespaces: map[string][]string{"cert-manager": {"cert-manager"}},
		},
	}

	results := c.RunOperatorChecks(context.Background())
	if len(results) == 0 {
		t.Fatal("expected at least one operator check result")
	}
	foundFailOrWarn := false
	for _, r := range results {
		if r.Status == checks.StatusFail || r.Status == checks.StatusWarn {
			foundFailOrWarn = true
		}
	}
	if !foundFailOrWarn {
		t.Errorf("expected a FAIL/WARN result for a namespace with no pods, got %+v", results)
	}
}

func TestRunOperatorChecks_HealthyPodsPass(t *testing.T) {
	client := fake.NewSimpleClientset( //nolint:staticcheck
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "cert-manager"}},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "cert-manager-abc123",
				Namespace: "cert-manager",
				Labels:    map[string]string{"app": "cert-manager"},
			},
			Status: corev1.PodStatus{
				Phase:      corev1.PodRunning,
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			},
		},
	)
	c, _ := newTestController(client)
	c.cfg = config.PlatformConfig{
		Operators: config.OperatorConfig{
			Namespaces: map[string][]string{"cert-manager": {"cert-manager"}},
		},
	}

	results := c.RunOperatorChecks(context.Background())
	found := false
	for _, r := range results {
		if r.Name != "cert-manager" {
			continue
		}
		found = true
		if r.Status != checks.StatusPass {
			t.Errorf("expected cert-manager check to PASS with a healthy pod present, got %s: %s", r.Status, r.Message)
		}
	}
	if !found {
		t.Fatal("expected a cert-manager result in the operator check output")
	}
}
