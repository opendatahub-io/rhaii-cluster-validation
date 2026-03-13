package jobrunner

import (
	"context"
	"fmt"
	"io"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Runner orchestrates server/client job lifecycle for multi-node tests.
type Runner struct {
	client    kubernetes.Interface
	namespace string
	image     string
	timeout   time.Duration
	output    io.Writer
}

// New creates a new job Runner.
func New(client kubernetes.Interface, namespace, image string, timeout time.Duration, output io.Writer) *Runner {
	return &Runner{
		client:    client,
		namespace: namespace,
		image:     image,
		timeout:   timeout,
		output:    output,
	}
}

// RunJob executes a multi-node job: deploys server, waits for IP, deploys clients,
// waits for completion, collects logs, parses results, and cleans up.
func (r *Runner) RunJob(ctx context.Context, job Job, serverNode string, clientNodes []string) ([]JobResult, error) {
	var createdJobs []*batchv1.Job
	defer func() {
		r.cleanup(context.Background(), createdJobs)
	}()

	// Step 1: Create server job
	fmt.Fprintf(r.output, "  [%s] Deploying server on %s...\n", job.Name(), serverNode)
	serverJob, err := job.ServerSpec(serverNode, r.namespace, r.image)
	if err != nil {
		return nil, fmt.Errorf("failed to build server job spec: %w", err)
	}
	created, err := r.client.BatchV1().Jobs(r.namespace).Create(ctx, serverJob, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to create server job: %w", err)
	}
	createdJobs = append(createdJobs, created)

	// Step 2: Wait for server pod to be running and get its IP
	fmt.Fprintf(r.output, "  [%s] Waiting for server pod IP...\n", job.Name())
	serverIP, err := r.waitForPodIP(ctx, created.Name)
	if err != nil {
		return nil, fmt.Errorf("server pod failed to start: %w", err)
	}
	fmt.Fprintf(r.output, "  [%s] Server running at %s\n", job.Name(), serverIP)

	// Give the server process time to start listening.
	// PodRunning only means the container started, not that the server is ready.
	select {
	case <-time.After(3 * time.Second):
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	// Step 3: Create client jobs
	for _, node := range clientNodes {
		fmt.Fprintf(r.output, "  [%s] Deploying client on %s → %s...\n", job.Name(), node, serverIP)
		clientJob, err := job.ClientSpec(node, r.namespace, r.image, serverIP)
		if err != nil {
			return nil, fmt.Errorf("failed to build client job spec for %s: %w", node, err)
		}
		created, err := r.client.BatchV1().Jobs(r.namespace).Create(ctx, clientJob, metav1.CreateOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to create client job on %s: %w", node, err)
		}
		createdJobs = append(createdJobs, created)
	}

	// Step 4: Wait for all client jobs to complete
	fmt.Fprintf(r.output, "  [%s] Waiting for %d client job(s) to complete...\n", job.Name(), len(clientNodes))
	if err := r.waitForJobs(ctx, createdJobs[1:]); err != nil {
		return nil, err
	}

	// Step 5: Collect logs and parse results from client jobs
	var results []JobResult
	for _, j := range createdJobs[1:] {
		logs, err := r.getJobLogs(ctx, j.Name)
		if err != nil {
			fmt.Fprintf(r.output, "  [%s] Warning: failed to get logs from %s: %v\n", job.Name(), j.Name, err)
			continue
		}

		result, err := job.ParseResult(logs)
		if err != nil {
			fmt.Fprintf(r.output, "  [%s] Warning: failed to parse result from %s: %v\n", job.Name(), j.Name, err)
			continue
		}

		// Fill in metadata: show "client → server" for cross-node tests
		clientNode := j.Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"]
		result.Node = fmt.Sprintf("%s → %s", clientNode, serverNode)
		result.Role = RoleClient
		result.JobName = job.Name()

		results = append(results, *result)
	}

	fmt.Fprintf(r.output, "  [%s] Collected %d result(s)\n", job.Name(), len(results))
	return results, nil
}

// waitForPodIP polls until a pod owned by the named job is Running and has a PodIP.
func (r *Runner) waitForPodIP(ctx context.Context, jobName string) (string, error) {
	timeout := time.After(r.timeout)
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	selector := fmt.Sprintf("job-name=%s", jobName)

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-timeout:
			return "", fmt.Errorf("timed out waiting for pod IP")
		case <-ticker.C:
			pods, err := r.client.CoreV1().Pods(r.namespace).List(ctx, metav1.ListOptions{
				LabelSelector: selector,
			})
			if err != nil || len(pods.Items) == 0 {
				continue
			}

			pod := pods.Items[0]
			if pod.Status.Phase == corev1.PodRunning && pod.Status.PodIP != "" {
				return pod.Status.PodIP, nil
			}
			if pod.Status.Phase == corev1.PodFailed {
				return "", fmt.Errorf("server pod failed")
			}
		}
	}
}

// waitForJobs polls until all jobs have completed (succeeded or failed).
func (r *Runner) waitForJobs(ctx context.Context, jobs []*batchv1.Job) error {
	timeout := time.After(r.timeout)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout:
			return fmt.Errorf("timed out waiting for jobs to complete")
		case <-ticker.C:
			done := 0
			for _, j := range jobs {
				current, err := r.client.BatchV1().Jobs(r.namespace).Get(ctx, j.Name, metav1.GetOptions{})
				if err != nil {
					continue
				}
				if current.Status.Succeeded > 0 || current.Status.Failed > 0 {
					done++
				}
			}
			fmt.Fprintf(r.output, "  Jobs completed: %d/%d\n", done, len(jobs))
			if done >= len(jobs) {
				return nil
			}
		}
	}
}

// getJobLogs returns the logs from the first pod of a job.
func (r *Runner) getJobLogs(ctx context.Context, jobName string) (string, error) {
	selector := fmt.Sprintf("job-name=%s", jobName)
	pods, err := r.client.CoreV1().Pods(r.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil || len(pods.Items) == 0 {
		return "", fmt.Errorf("no pods found for job %s", jobName)
	}

	req := r.client.CoreV1().Pods(r.namespace).GetLogs(pods.Items[0].Name, &corev1.PodLogOptions{})
	stream, err := req.Stream(ctx)
	if err != nil {
		return "", err
	}
	defer stream.Close()

	data, err := io.ReadAll(stream)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// cleanup deletes all created jobs and their pods.
func (r *Runner) cleanup(ctx context.Context, jobs []*batchv1.Job) {
	propagation := metav1.DeletePropagationForeground
	for _, j := range jobs {
		err := r.client.BatchV1().Jobs(r.namespace).Delete(ctx, j.Name, metav1.DeleteOptions{
			PropagationPolicy: &propagation,
		})
		if err != nil && !apierrors.IsNotFound(err) {
			fmt.Fprintf(r.output, "  Warning: failed to cleanup job %s: %v\n", j.Name, err)
		}
	}
}
