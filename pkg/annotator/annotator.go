package annotator

import (
	"context"
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	// AnnotationKey is the annotation used to track validation status on agent pods.
	AnnotationKey = "rhaii.opendatahub.io/validation-status"

	StatusStarting = "starting"
	StatusRunning  = "running"
	StatusDone     = "done"
	StatusError    = "error"
)

// Annotator updates annotations on the agent's own pod.
type Annotator struct {
	client    kubernetes.Interface
	podName   string
	namespace string
}

// NewWithClient creates an Annotator with an injected Kubernetes client.
func NewWithClient(client kubernetes.Interface, podName, namespace string) *Annotator {
	return &Annotator{
		client:    client,
		podName:   podName,
		namespace: namespace,
	}
}

// New creates an Annotator using in-cluster config.
func New(podName, namespace string) (*Annotator, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get in-cluster config: %w", err)
	}

	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes client: %w", err)
	}

	return NewWithClient(client, podName, namespace), nil
}

// SetStatus updates the validation-status annotation on the agent's pod.
func (a *Annotator) SetStatus(ctx context.Context, status string) error {
	patch := map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": map[string]string{
				AnnotationKey: status,
			},
		},
	}

	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("failed to marshal patch: %w", err)
	}

	_, err = a.client.CoreV1().Pods(a.namespace).Patch(
		ctx, a.podName, types.MergePatchType, patchBytes, metav1.PatchOptions{},
	)
	if err != nil {
		return fmt.Errorf("failed to patch pod %s: %w", a.podName, err)
	}

	return nil
}
