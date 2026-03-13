package jobrunner

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BuildJobSpec creates a base K8s Job with common settings.
// Job implementations call this then customize the container args.
func BuildJobSpec(name, node, namespace, image string, role Role, podCfg *PodConfig, command []string) (*batchv1.Job, error) {
	labels := map[string]string{
		"app":            "rhaii-validate-job",
		"rhaii-job-type": name,
		"rhaii-role":     string(role),
	}

	var backoffLimit int32 = 0

	jobName := fmt.Sprintf("%s-%s-%s", name, role, node)
	if len(jobName) > 63 {
		h := sha256.Sum256([]byte(jobName))
		suffix := hex.EncodeToString(h[:3])
		jobName = jobName[:56] + "-" + suffix
	}
	jobName = strings.TrimRight(jobName, "-")

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
					Tolerations: []corev1.Toleration{
						{Operator: corev1.TolerationOpExists},
					},
					Containers:         []corev1.Container{container},
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: "rhaii-validate-agent",
				},
			},
		},
	}, nil
}
