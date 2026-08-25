package controller

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestPrintDebugHelp_ListsJobsAndPods(t *testing.T) {
	client := fake.NewSimpleClientset( //nolint:staticcheck
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{
			Name: "rhaii-validate-check-node1", Namespace: "test-ns",
			Labels: map[string]string{checkJobLabelKey: gpuCheckJobLabelValue},
		}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: "rhaii-validate-check-node1-xyz", Namespace: "test-ns",
			Labels: map[string]string{checkJobLabelKey: gpuCheckJobLabelValue},
		}, Spec: corev1.PodSpec{NodeName: "node1"},
			Status: corev1.PodStatus{Phase: corev1.PodRunning}},
	)
	c, buf := newTestController(client)

	c.printDebugHelp(context.Background())

	out := buf.String()
	if !strings.Contains(out, "DEBUG MODE") {
		t.Errorf("expected debug banner, got: %s", out)
	}
	if !strings.Contains(out, "rhaii-validate-check-node1") {
		t.Errorf("expected job name in output, got: %s", out)
	}
	if !strings.Contains(out, "kubectl logs -n test-ns rhaii-validate-check-node1-xyz") {
		t.Errorf("expected pod log command in output, got: %s", out)
	}
}

func TestPrintDebugHelp_NoResourcesStillPrintsBanner(t *testing.T) {
	c, buf := newTestController(nil)

	c.printDebugHelp(context.Background())

	if !strings.Contains(buf.String(), "DEBUG MODE") {
		t.Errorf("expected debug banner even with no jobs/pods, got: %s", buf.String())
	}
}

func TestPrintDebugCleanupHint(t *testing.T) {
	c, buf := newTestController(nil)
	c.opts.Namespace = "my-ns"

	c.printDebugCleanupHint()

	out := buf.String()
	for _, want := range []string{
		"kubectl delete clusterrolebinding rhaii-validator",
		"kubectl delete clusterrole rhaii-validator",
		"kubectl delete serviceaccount -n my-ns rhaii-validator",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q, got: %s", want, out)
		}
	}
}
