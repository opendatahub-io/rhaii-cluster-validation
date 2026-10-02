package operator

import (
	"context"
	"testing"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func runningPod(name, ns string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func TestCheckOperator_MissingRequiredFails(t *testing.T) {
	client := fake.NewSimpleClientset()
	c := &Checker{client: client, operators: []OperatorSpec{
		{Name: "cert-manager", Description: "cert-manager", Namespaces: []string{"cert-manager"}},
	}}

	got := c.checkOperator(context.Background(), c.operators[0])
	if got.Status != checks.StatusFail {
		t.Errorf("missing required operator: got %s, want %s", got.Status, checks.StatusFail)
	}
}

func TestCheckOperator_MissingOptionalWarns(t *testing.T) {
	client := fake.NewSimpleClientset()
	c := &Checker{client: client, operators: []OperatorSpec{
		{Name: "lws", Description: "LeaderWorkerSet operator", Namespaces: []string{"openshift-lws-operator"}, Optional: true},
	}}

	got := c.checkOperator(context.Background(), c.operators[0])
	if got.Status != checks.StatusWarn {
		t.Errorf("missing optional operator: got %s, want %s", got.Status, checks.StatusWarn)
	}
}

func TestCheckOperator_PresentHealthyPasses(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "openshift-lws-operator"}},
		runningPod("lws-controller", "openshift-lws-operator"),
	)
	c := &Checker{client: client, operators: []OperatorSpec{
		{Name: "lws", Description: "LeaderWorkerSet operator", Namespaces: []string{"openshift-lws-operator"}, Optional: true},
	}}

	got := c.checkOperator(context.Background(), c.operators[0])
	if got.Status != checks.StatusPass {
		t.Errorf("present healthy optional operator: got %s, want %s", got.Status, checks.StatusPass)
	}
}

func TestCheckOperator_OptionalInstalledButUnhealthyWarns(t *testing.T) {
	// Namespace exists but no running pods: an optional operator should not
	// hard-fail the cluster.
	client := fake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "openshift-lws-operator"}},
	)
	c := &Checker{client: client, operators: []OperatorSpec{
		{Name: "lws", Description: "LeaderWorkerSet operator", Namespaces: []string{"openshift-lws-operator"}, Optional: true},
	}}

	got := c.checkOperator(context.Background(), c.operators[0])
	if got.Status != checks.StatusWarn {
		t.Errorf("optional operator namespace with no pods: got %s, want %s", got.Status, checks.StatusWarn)
	}
}

func TestLWSOperatorIsOptional(t *testing.T) {
	for _, op := range RequiredOperators {
		if op.Name == "lws" {
			if !op.Optional {
				t.Errorf("lws operator should be Optional in RequiredOperators")
			}
			return
		}
	}
	t.Fatal("lws operator not found in RequiredOperators")
}
