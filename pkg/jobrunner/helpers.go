package jobrunner

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ApplyResourceConfig parses requests/limits (string quantity maps, e.g. from
// platform config) and merges them into container's resource requirements.
// Existing entries in the container's Resources are preserved; entries with
// the same resource name are overwritten. Initializes the Requests/Limits
// maps if needed.
func ApplyResourceConfig(container *corev1.Container, requests, limits map[string]string) error {
	if len(requests) > 0 {
		if container.Resources.Requests == nil {
			container.Resources.Requests = make(corev1.ResourceList)
		}
		for k, v := range requests {
			qty, err := resource.ParseQuantity(v)
			if err != nil {
				return fmt.Errorf("invalid resource request %q for %s: %w", v, k, err)
			}
			container.Resources.Requests[corev1.ResourceName(k)] = qty
		}
	}
	if len(limits) > 0 {
		if container.Resources.Limits == nil {
			container.Resources.Limits = make(corev1.ResourceList)
		}
		for k, v := range limits {
			qty, err := resource.ParseQuantity(v)
			if err != nil {
				return fmt.Errorf("invalid resource limit %q for %s: %w", v, k, err)
			}
			container.Resources.Limits[corev1.ResourceName(k)] = qty
		}
	}
	return nil
}

// SetGPUResource requests count units of gpuResource on container, initializing
// the Requests/Limits maps if needed. No-op if count <= 0 or gpuResource is empty.
func SetGPUResource(container *corev1.Container, gpuResource corev1.ResourceName, count int64) {
	if count <= 0 || gpuResource == "" {
		return
	}
	qty := resource.MustParse(fmt.Sprintf("%d", count))
	if container.Resources.Requests == nil {
		container.Resources.Requests = make(corev1.ResourceList)
	}
	if container.Resources.Limits == nil {
		container.Resources.Limits = make(corev1.ResourceList)
	}
	container.Resources.Requests[gpuResource] = qty
	container.Resources.Limits[gpuResource] = qty
}

// BuildJobSpec creates a base K8s Job with common settings.
// Job implementations call this then customize the container args.
func BuildJobSpec(name, node, namespace, image string, role Role, podCfg *PodConfig, command []string) (*batchv1.Job, error) {
	labels := map[string]string{
		"app":            "rhaii-validate-job",
		"rhaii-job-type": name,
		"rhaii-role":     string(role),
	}

	var backoffLimit int32 = 0
	noMount := false

	jobName := fmt.Sprintf("%s-%s-%s", name, role, node)
	if podCfg != nil && podCfg.NameSuffix != "" {
		jobName = fmt.Sprintf("%s-%s", jobName, podCfg.NameSuffix)
	}
	if len(jobName) > 63 {
		h := sha256.Sum256([]byte(jobName))
		suffix := hex.EncodeToString(h[:3])
		prefix := strings.TrimRight(jobName[:56], "-.")
		if prefix == "" {
			prefix = "job"
		}
		jobName = prefix + "-" + suffix
	}
	jobName = strings.TrimRight(jobName, "-.")

	container := corev1.Container{
		Name:    "job",
		Image:   image,
		Command: command,
	}

	// Apply pod configuration
	if podCfg != nil {
		reqs, err := podCfg.ToResourceRequirements()
		if err != nil {
			return nil, err
		}
		container.Resources = reqs
		if podCfg.Privileged {
			privileged := true
			container.SecurityContext = &corev1.SecurityContext{
				Privileged: &privileged,
			}
		}
	}

	annotations := map[string]string{}
	if podCfg != nil {
		for k, v := range podCfg.Annotations {
			annotations[k] = v
		}
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: annotations,
				},
				Spec: corev1.PodSpec{
					NodeSelector: map[string]string{
						"kubernetes.io/hostname": node,
					},
					AutomountServiceAccountToken: &noMount,
					// Universal toleration: GPU nodes carry platform-specific taints
					// that we cannot enumerate. The nodeSelector pins the pod to a
					// specific GPU node; this toleration prevents taint rejection.
					Tolerations: []corev1.Toleration{
						{Operator: corev1.TolerationOpExists},
					},
					Containers:         []corev1.Container{container},
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: "rhaii-validator",
				},
			},
		},
	}, nil
}
