package annotator

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestSetStatus(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-abc",
			Namespace: "rhaii-validation",
		},
	}
	client := fake.NewSimpleClientset(pod)
	ann := NewWithClient(client, "agent-abc", "rhaii-validation")
	ctx := context.Background()

	// Set starting
	if err := ann.SetStatus(ctx, StatusStarting); err != nil {
		t.Fatalf("SetStatus(starting) error: %v", err)
	}
	got, _ := client.CoreV1().Pods("rhaii-validation").Get(ctx, "agent-abc", metav1.GetOptions{})
	if v := got.Annotations[AnnotationKey]; v != StatusStarting {
		t.Errorf("annotation = %q, want %q", v, StatusStarting)
	}

	// Update to running
	if err := ann.SetStatus(ctx, StatusRunning); err != nil {
		t.Fatalf("SetStatus(running) error: %v", err)
	}
	got, _ = client.CoreV1().Pods("rhaii-validation").Get(ctx, "agent-abc", metav1.GetOptions{})
	if v := got.Annotations[AnnotationKey]; v != StatusRunning {
		t.Errorf("annotation = %q, want %q", v, StatusRunning)
	}

	// Update to done
	if err := ann.SetStatus(ctx, StatusDone); err != nil {
		t.Fatalf("SetStatus(done) error: %v", err)
	}
	got, _ = client.CoreV1().Pods("rhaii-validation").Get(ctx, "agent-abc", metav1.GetOptions{})
	if v := got.Annotations[AnnotationKey]; v != StatusDone {
		t.Errorf("annotation = %q, want %q", v, StatusDone)
	}
}

func TestSetStatusNonExistentPod(t *testing.T) {
	client := fake.NewSimpleClientset() // no pods
	ann := NewWithClient(client, "no-such-pod", "rhaii-validation")

	err := ann.SetStatus(context.Background(), StatusStarting)
	if err == nil {
		t.Fatal("expected error for non-existent pod, got nil")
	}
}
