package jobrunner

import (
	"fmt"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Role represents the role of a job pod in a multi-node test.
type Role string

const (
	RoleServer Role = "server"
	RoleClient Role = "client"
)

// Job defines a multi-node test workload (e.g. iperf3, ib_write_bw, NCCL).
type Job interface {
	// Name returns a short identifier for the job type (e.g. "iperf3-tcp").
	Name() string

	// ServerSpec builds a K8s Job spec for the server role on the given node.
	ServerSpec(node, namespace, image string) (*batchv1.Job, error)

	// ClientSpec builds a K8s Job spec for the client role, connecting to serverIP.
	ClientSpec(node, namespace, image, serverIP string) (*batchv1.Job, error)

	// ParseResult parses the job's stdout logs into a structured result.
	ParseResult(logs string) (*JobResult, error)
}

// PodConfig holds optional pod-level configuration for job pods.
// Can be set per-job or loaded from the platform config YAML.
type PodConfig struct {
	Annotations      map[string]string `json:"annotations,omitempty" yaml:"annotations,omitempty"`
	ResourceRequests map[string]string `json:"resourceRequests,omitempty" yaml:"resourceRequests,omitempty"`
	ResourceLimits   map[string]string `json:"resourceLimits,omitempty" yaml:"resourceLimits,omitempty"`
	Privileged       bool             `json:"privileged,omitempty" yaml:"privileged,omitempty"`
}

// ToResourceRequirements converts PodConfig resource maps to K8s ResourceRequirements.
func (pc *PodConfig) ToResourceRequirements() (corev1.ResourceRequirements, error) {
	reqs := corev1.ResourceRequirements{}
	if len(pc.ResourceRequests) > 0 {
		reqs.Requests = make(corev1.ResourceList)
		for k, v := range pc.ResourceRequests {
			qty, err := resource.ParseQuantity(v)
			if err != nil {
				return corev1.ResourceRequirements{}, fmt.Errorf("invalid resource value %q for %s: %w", v, k, err)
			}
			reqs.Requests[corev1.ResourceName(k)] = qty
		}
	}
	if len(pc.ResourceLimits) > 0 {
		reqs.Limits = make(corev1.ResourceList)
		for k, v := range pc.ResourceLimits {
			qty, err := resource.ParseQuantity(v)
			if err != nil {
				return corev1.ResourceRequirements{}, fmt.Errorf("invalid resource value %q for %s: %w", v, k, err)
			}
			reqs.Limits[corev1.ResourceName(k)] = qty
		}
	}
	return reqs, nil
}

// ThresholdConfigurable is an optional interface for jobs that accept a threshold.
type ThresholdConfigurable interface {
	SetThreshold(float64)
}

// Configurable is an optional interface for jobs that accept a PodConfig.
type Configurable interface {
	SetPodConfig(cfg *PodConfig)
}

// JobResult holds the outcome of a single job pod.
type JobResult struct {
	Node    string        `json:"node"`
	Role    Role          `json:"role"`
	JobName string        `json:"job_name"`
	Status  checks.Status `json:"status"`
	Message string        `json:"message"`
	Details any           `json:"details,omitempty"`
}
