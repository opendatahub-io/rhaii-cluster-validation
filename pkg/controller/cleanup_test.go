package controller

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestDeleteJobsBySelector_NoMatchingJobs(t *testing.T) {
	c, _ := newTestController(nil)

	if ok := c.deleteJobsBySelector(context.Background(), "app=does-not-exist"); !ok {
		t.Error("expected deleteJobsBySelector() to return true when no jobs match")
	}
}

func TestDeleteJobsBySelector_DeletesMatchingJobsOnly(t *testing.T) {
	client := fake.NewSimpleClientset( //nolint:staticcheck
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "keep-me", Namespace: "test-ns", Labels: map[string]string{"app": "other"}}},
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "delete-me", Namespace: "test-ns", Labels: map[string]string{"app": "rhaii-validate-gpu-check"}}},
	)
	c, _ := newTestController(client)

	if ok := c.deleteJobsBySelector(context.Background(), checkJobLabelKey+"="+gpuCheckJobLabelValue); !ok {
		t.Error("expected deleteJobsBySelector() to return true (no pods block termination)")
	}

	jobs, err := client.BatchV1().Jobs("test-ns").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("failed to list jobs: %v", err)
	}
	if len(jobs.Items) != 1 || jobs.Items[0].Name != "keep-me" {
		t.Errorf("expected only 'keep-me' to remain, got %+v", jobs.Items)
	}
}

func TestCleanupHelpers_UseExpectedSelectors(t *testing.T) {
	tests := []struct {
		name    string
		cleanup func(*Controller, context.Context) bool
		label   string
	}{
		{name: "gpu check", cleanup: (*Controller).cleanupGpuCheckJobs, label: checkJobLabelKey + "=" + gpuCheckJobLabelValue},
		{name: "net check", cleanup: (*Controller).cleanupNetCheckJobs, label: checkJobLabelKey + "=" + netCheckJobLabelValue},
		{name: "bw probe", cleanup: (*Controller).cleanupLoopbackBWProbeJobs, label: checkJobLabelKey + "=" + bwProbeLabelValue},
		{name: "bandwidth", cleanup: (*Controller).cleanupBandwidthJobs, label: "app=rhaii-validate-job"},
		{name: "pingmesh", cleanup: (*Controller).cleanupPingMeshJobs, label: "rhaii-job-type=pingmesh"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k, v, _ := splitSelector(tt.label)
			job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
				Name: "target", Namespace: "test-ns", Labels: map[string]string{k: v},
			}}
			other := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
				Name: "bystander", Namespace: "test-ns", Labels: map[string]string{"app": "unrelated"},
			}}
			client := fake.NewSimpleClientset(job, other) //nolint:staticcheck
			c, _ := newTestController(client)

			tt.cleanup(c, context.Background())

			jobs, err := client.BatchV1().Jobs("test-ns").List(context.Background(), metav1.ListOptions{})
			if err != nil {
				t.Fatalf("failed to list jobs: %v", err)
			}
			if len(jobs.Items) != 1 || jobs.Items[0].Name != "bystander" {
				t.Errorf("expected only 'bystander' to remain, got %+v", jobs.Items)
			}
		})
	}
}

// splitSelector splits a "key=value" label selector into its parts for test setup.
func splitSelector(selector string) (key, value string, ok bool) {
	for i := 0; i < len(selector); i++ {
		if selector[i] == '=' {
			return selector[:i], selector[i+1:], true
		}
	}
	return selector, "", false
}

func TestCleanupAll_RemovesRBACButPreservesConfigMap(t *testing.T) {
	client := fake.NewSimpleClientset( //nolint:staticcheck
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "rhaii-validator", Namespace: "test-ns"}},
		&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "rhaii-validator"}},
		&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "rhaii-validator"}},
	)
	c, _ := newTestController(client)

	if err := c.cleanupAll(context.Background()); err != nil {
		t.Fatalf("cleanupAll() error = %v", err)
	}

	if _, err := client.CoreV1().ServiceAccounts(c.opts.Namespace).Get(context.Background(), "rhaii-validator", metav1.GetOptions{}); err == nil {
		t.Error("expected ServiceAccount to be deleted")
	}
	if _, err := client.RbacV1().ClusterRoles().Get(context.Background(), "rhaii-validator", metav1.GetOptions{}); err == nil {
		t.Error("expected ClusterRole to be deleted")
	}

	// Calling again (nothing left to delete) must not error.
	if err := c.cleanupAll(context.Background()); err != nil {
		t.Fatalf("cleanupAll() second call error = %v", err)
	}
}
