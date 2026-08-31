package jobrunner

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// syncWriter wraps an io.Writer with a mutex for concurrent use.
// Used by RunPairwise so child runners can safely share the parent's output
// even when it's a non-thread-safe writer like bytes.Buffer (e.g. in tests).
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (sw *syncWriter) Write(p []byte) (int, error) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.w.Write(p)
}

// NodePair identifies a server/client node combination for pairwise testing.
type NodePair struct {
	Server string
	Client string
}

// Runner orchestrates server/client job lifecycle for multi-node tests.
type Runner struct {
	client        kubernetes.Interface
	namespace     string
	image         string
	timeout       time.Duration
	output        io.Writer
	debug         bool
	quietProgress bool // suppress polling/completion output (used by RunPairwise)
}

// New creates a new job Runner.
func New(client kubernetes.Interface, namespace, image string, timeout time.Duration, output io.Writer, debug bool) *Runner {
	return &Runner{
		client:    client,
		namespace: namespace,
		image:     image,
		timeout:   timeout,
		output:    output,
		debug:     debug,
	}
}

// RunRing runs all jobs in a ring topology: each node is server once, next node is client.
// Tests every node as both sender and receiver.
func (r *Runner) RunRing(ctx context.Context, jobs []Job, nodes []string) ([]JobResult, error) {
	fmt.Fprintf(r.output, "  Mode: ring (cross-node, %d pairs)\n", len(nodes))
	for i := 0; i < len(nodes); i++ {
		client := nodes[(i+1)%len(nodes)]
		fmt.Fprintf(r.output, "    Pair %d: %s → %s\n", i+1, client, nodes[i])
	}

	var allResults []JobResult
	for i := 0; i < len(nodes); i++ {
		server := nodes[i]
		client := nodes[(i+1)%len(nodes)]
		fmt.Fprintf(r.output, "\n  --- Round %d/%d: server=%s ---\n", i+1, len(nodes), server)

		results, err := r.runJobsOnPair(ctx, jobs, server, []string{client})
		if err != nil {
			return allResults, err
		}
		allResults = append(allResults, results...)
	}
	return allResults, nil
}

// RunStar runs all jobs in a star topology: one server, all others are clients.
func (r *Runner) RunStar(ctx context.Context, jobs []Job, serverNode string, clientNodes []string) ([]JobResult, error) {
	fmt.Fprintf(r.output, "  Mode: star\n")
	fmt.Fprintf(r.output, "  Server: %s, Clients: %s\n", serverNode, strings.Join(clientNodes, ", "))
	return r.runJobsOnPair(ctx, jobs, serverNode, clientNodes)
}

// RunPairwise runs N-choose-2 node pairs using round-robin tournament scheduling.
// Disjoint pairs within each round run in parallel. Each pair is tried up to
// maxAttempts times. Returns results keyed by NodePair for caller classification.
func (r *Runner) RunPairwise(ctx context.Context, jobs map[NodePair]Job, maxAttempts int) (map[NodePair][]JobResult, error) {
	if maxAttempts < 1 {
		return nil, fmt.Errorf("maxAttempts must be >= 1")
	}
	// Extract unique nodes
	nodeSet := make(map[string]bool)
	for pair := range jobs {
		nodeSet[pair.Server] = true
		nodeSet[pair.Client] = true
	}
	nodes := make([]string, 0, len(nodeSet))
	for n := range nodeSet {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)

	schedule := roundRobinSchedule(nodes)
	fmt.Fprintf(r.output, "  Mode: pairwise (%d pairs, %d rounds)\n", len(jobs), len(schedule))

	// Wrap output so concurrent goroutines can write safely, even if the
	// underlying writer is not thread-safe (e.g. bytes.Buffer in tests).
	safeOut := &syncWriter{w: r.output}

	results := make(map[NodePair][]JobResult)
	var mu sync.Mutex

	for roundIdx, round := range schedule {
		fmt.Fprintf(safeOut, "\n  --- Round %d/%d (%d parallel pairs) ---\n", roundIdx+1, len(schedule), len(round))

		var activePairs []NodePair
		var wg sync.WaitGroup
		for _, pair := range round {
			job, ok := jobs[pair]
			if !ok {
				continue
			}
			activePairs = append(activePairs, pair)
			wg.Add(1)
			// Each goroutine receives its own Job from jobMap (no sharing).
			// SetNameSuffix mutates the job, which is safe because pairs are disjoint per round.
			go func(p NodePair, j Job, ri int) {
				defer wg.Done()

				qr := &Runner{
					client:        r.client,
					namespace:     r.namespace,
					image:         r.image,
					timeout:       r.timeout,
					output:        safeOut,
					debug:         r.debug,
					quietProgress: true,
				}

				for attempt := 1; attempt <= maxAttempts; attempt++ {
					if ctx.Err() != nil {
						break
					}
					if ns, ok := j.(NameSuffixable); ok {
						ns.SetNameSuffix(fmt.Sprintf("r%da%d", ri+1, attempt))
					}

					jr, err := qr.RunJob(ctx, j, p.Server, []string{p.Client})
					if err != nil {
						mu.Lock()
						results[p] = append(results[p], JobResult{
							JobName: j.Name(),
							Node:    fmt.Sprintf("%s → %s", p.Client, p.Server),
							Role:    RoleClient,
							Status:  checks.StatusFail,
							Message: fmt.Sprintf("job failed: %v", err),
						})
						mu.Unlock()
						continue
					}

					mu.Lock()
					results[p] = append(results[p], jr...)
					mu.Unlock()

					allPass := true
					for _, res := range jr {
						if res.Status != checks.StatusPass {
							allPass = false
							break
						}
					}
					if allPass {
						break
					}
				}
			}(pair, job, roundIdx)
		}
		wg.Wait()

		// Round summary
		passed, failed := 0, 0
		for _, p := range activePairs {
			mu.Lock()
			pairResults := results[p]
			mu.Unlock()
			pairOK := false
			for _, jr := range pairResults {
				if jr.Status == checks.StatusPass {
					pairOK = true
					break
				}
			}
			if pairOK {
				passed++
			} else {
				failed++
			}
		}
		fmt.Fprintf(safeOut, "  Round %d complete: %d/%d pairs passed\n", roundIdx+1, passed, len(activePairs))
	}

	return results, nil
}

// roundRobinSchedule generates a round-robin tournament for N nodes.
// Returns rounds of disjoint NodePairs. For odd N, one node sits out per round.
func roundRobinSchedule(nodes []string) [][]NodePair {
	n := len(nodes)
	if n < 2 {
		return nil
	}

	// For round-robin we need an even number; add a "bye" (empty string) if odd
	working := make([]string, len(nodes))
	copy(working, nodes)
	if n%2 != 0 {
		working = append(working, "")
	}
	m := len(working)

	var rounds [][]NodePair
	for round := 0; round < m-1; round++ {
		var pairs []NodePair
		for i := 0; i < m/2; i++ {
			a := working[i]
			b := working[m-1-i]
			if a == "" || b == "" {
				continue
			}
			// Consistent ordering: lexicographically smaller is Server
			if a > b {
				a, b = b, a
			}
			pairs = append(pairs, NodePair{Server: a, Client: b})
		}
		if len(pairs) > 0 {
			rounds = append(rounds, pairs)
		}
		// Rotate: fix first element, rotate rest
		last := working[m-1]
		copy(working[2:], working[1:m-1])
		working[1] = last
	}
	return rounds
}

// runJobsOnPair runs all jobs for a single server/client pair.
func (r *Runner) runJobsOnPair(ctx context.Context, jobs []Job, serverNode string, clientNodes []string) ([]JobResult, error) {
	var allResults []JobResult
	for _, job := range jobs {
		fmt.Fprintf(r.output, "  Running job: %s (%s → %s)\n", job.Name(), strings.Join(clientNodes, ","), serverNode)
		results, err := r.RunJob(ctx, job, serverNode, clientNodes)
		if err != nil {
			fmt.Fprintf(r.output, "  Warning: job %s failed: %v\n", job.Name(), err)
			allResults = append(allResults, JobResult{
				JobName: job.Name(),
				Node:    fmt.Sprintf("%s → %s", strings.Join(clientNodes, ","), serverNode),
				Role:    RoleClient,
				Status:  checks.StatusFail,
				Message: fmt.Sprintf("job failed: %v", err),
			})
			continue
		}
		allResults = append(allResults, results...)
	}
	return allResults, nil
}

// RunJob executes a multi-node job: deploys server, waits for IP, deploys clients,
// waits for completion, collects logs, parses results, and cleans up.
func (r *Runner) RunJob(ctx context.Context, job Job, serverNode string, clientNodes []string) ([]JobResult, error) {
	var createdJobs []*batchv1.Job
	defer func() {
		if !r.debug {
			r.cleanup(context.Background(), createdJobs)
		}
	}()

	// Step 1: Create server job
	fmt.Fprintf(r.output, "  [%s] Deploying server on %s...\n", job.Name(), serverNode)

	// Determine which image to use for server
	serverImage := r.image
	if imgConfig, ok := job.(ImageConfigurable); ok {
		if customImg := imgConfig.GetServerImage(); customImg != "" {
			serverImage = customImg
			fmt.Fprintf(r.output, "  [%s] Using custom server image: %s\n", job.Name(), serverImage)
		}
	}

	serverJob, err := job.ServerSpec(serverNode, r.namespace, serverImage)
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
		if logs, logErr := r.getJobLogs(ctx, created.Name); logErr == nil && logs != "" {
			return nil, fmt.Errorf("server pod failed: %s", strings.TrimSpace(logs))
		}
		return nil, fmt.Errorf("server pod failed to start: %w", err)
	}
	fmt.Fprintf(r.output, "  [%s] Server running at %s (%s)\n", job.Name(), serverNode, serverIP)

	// Give the server process time to start listening.
	// PodRunning only means the container started, not that the server is ready.
	select {
	case <-time.After(3 * time.Second):
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	// Step 3: Create client jobs
	for _, node := range clientNodes {
		// Determine which image to use for client
		clientImage := r.image
		if imgConfig, ok := job.(ImageConfigurable); ok {
			if customImg := imgConfig.GetClientImage(); customImg != "" {
				clientImage = customImg
				fmt.Fprintf(r.output, "  [%s] Using custom client image: %s\n", job.Name(), clientImage)
			}
		}

		fmt.Fprintf(r.output, "  [%s] Deploying client on %s → %s...\n", job.Name(), node, serverIP)
		clientJob, err := job.ClientSpec(node, r.namespace, clientImage, serverIP)
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
		clientNode := j.Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"]
		nodeDesc := fmt.Sprintf("%s → %s", clientNode, serverNode)

		logs, err := r.getJobLogs(ctx, j.Name)
		if err != nil {
			fmt.Fprintf(r.output, "  [%s] Warning: failed to get logs from %s: %v\n", job.Name(), j.Name, err)
			results = append(results, JobResult{
				JobName: job.Name(),
				Node:    nodeDesc,
				Role:    RoleClient,
				Status:  checks.StatusFail,
				Message: fmt.Sprintf("failed to get logs: %v", err),
			})
			continue
		}

		result, err := job.ParseResult(logs)
		if err != nil {
			fmt.Fprintf(r.output, "  [%s] Warning: failed to parse result from %s: %v\n", job.Name(), j.Name, err)
			results = append(results, JobResult{
				JobName: job.Name(),
				Node:    nodeDesc,
				Role:    RoleClient,
				Status:  checks.StatusFail,
				Message: fmt.Sprintf("failed to parse result: %v", err),
			})
			continue
		}

		result.Node = nodeDesc
		result.Role = RoleClient
		result.JobName = job.Name()

		results = append(results, *result)
	}

	if !r.quietProgress {
		fmt.Fprintf(r.output, "  [%s] Collected %d result(s)\n", job.Name(), len(results))
	}
	return results, nil
}

// waitForPodIP polls until a pod owned by the named job is Running and has a PodIP.
// If the pod is stuck Pending due to scheduling issues, it reports the reason early.
func (r *Runner) waitForPodIP(ctx context.Context, jobName string) (string, error) {
	timeout := time.After(r.timeout)
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	selector := fmt.Sprintf("job-name=%s", jobName)
	schedulingErrorReported := false

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-timeout:
			// On timeout, try to get a useful error message
			if reason := r.getPodSchedulingError(ctx, selector); reason != "" {
				return "", fmt.Errorf("timed out waiting for pod to schedule:\n  %s", reason)
			}
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

			// Check for scheduling problems while pod is Pending
			if pod.Status.Phase == corev1.PodPending && !schedulingErrorReported {
				if reason := r.getPodSchedulingError(ctx, selector); reason != "" {
					fmt.Fprintf(r.output, "  WARNING: Pod %s is pending: %s\n", pod.Name, reason)
					schedulingErrorReported = true
				}
			}
		}
	}
}

// waitForJobs polls until all jobs have completed (succeeded or failed).
// Reports scheduling issues for any pending pods.
func (r *Runner) waitForJobs(ctx context.Context, jobs []*batchv1.Job) error {
	timeout := time.After(r.timeout)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	reportedPending := make(map[string]bool)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout:
			// On timeout, collect scheduling errors from all pending jobs
			var pendingErrors []string
			for _, j := range jobs {
				selector := fmt.Sprintf("job-name=%s", j.Name)
				if reason := r.getPodSchedulingError(ctx, selector); reason != "" {
					pendingErrors = append(pendingErrors, fmt.Sprintf("  %s: %s", j.Name, reason))
				}
			}
			if len(pendingErrors) > 0 {
				return fmt.Errorf("timed out waiting for jobs to complete. Scheduling errors:\n%s",
					strings.Join(pendingErrors, "\n"))
			}
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
					continue
				}

				// Check for pending pods with scheduling issues
				if !reportedPending[j.Name] {
					selector := fmt.Sprintf("job-name=%s", j.Name)
					if reason := r.getPodSchedulingError(ctx, selector); reason != "" {
						fmt.Fprintf(r.output, "  WARNING: Job %s pending: %s\n", j.Name, reason)
						reportedPending[j.Name] = true
					}
				}
			}
			if !r.quietProgress {
				fmt.Fprintf(r.output, "  Jobs completed: %d/%d\n", done, len(jobs))
			}
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

// getPodSchedulingError checks pod events for scheduling failures and returns a human-readable reason.
// Returns empty string if no scheduling issues found.
func (r *Runner) getPodSchedulingError(ctx context.Context, selector string) string {
	pods, err := r.client.CoreV1().Pods(r.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil || len(pods.Items) == 0 {
		return ""
	}

	pod := pods.Items[0]
	if pod.Status.Phase != corev1.PodPending {
		return ""
	}

	// Check pod conditions for scheduling failures
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse {
			reason := cond.Message
			if strings.Contains(reason, "Insufficient") {
				return fmt.Sprintf("Insufficient resources: %s\n  Suggestion: Reduce resource requests in ConfigMap or free up resources on nodes\n  Fix: kubectl edit configmap rhaii-validate-config -n %s", reason, r.namespace)
			}
			if strings.Contains(reason, "node(s) didn't match") || strings.Contains(reason, "MatchNodeSelector") {
				return fmt.Sprintf("No matching nodes: %s\n  Suggestion: Check node labels and taints", reason)
			}
			return reason
		}
	}

	// Check events for FailedScheduling
	events, err := r.client.CoreV1().Events(r.namespace).List(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("involvedObject.name=%s,reason=FailedScheduling", pod.Name),
	})
	if err != nil || len(events.Items) == 0 {
		return ""
	}

	event := events.Items[len(events.Items)-1]
	msg := event.Message
	if strings.Contains(msg, "Insufficient") {
		return fmt.Sprintf("Insufficient resources: %s\n  Suggestion: Reduce resource requests in ConfigMap or free up resources on nodes\n  Fix: kubectl edit configmap rhaii-validate-config -n %s", msg, r.namespace)
	}
	return msg
}

// cleanup deletes all created jobs and their pods, then waits for pod
// termination so extended resources (e.g. EFA) are released before retries.
func (r *Runner) cleanup(ctx context.Context, jobs []*batchv1.Job) {
	propagation := metav1.DeletePropagationBackground
	for _, j := range jobs {
		err := r.client.BatchV1().Jobs(r.namespace).Delete(ctx, j.Name, metav1.DeleteOptions{
			PropagationPolicy: &propagation,
		})
		if err != nil && !apierrors.IsNotFound(err) {
			fmt.Fprintf(r.output, "  Warning: failed to cleanup job %s: %v\n", j.Name, err)
		}
	}

	for i := 0; i < 60; i++ {
		allGone := true
		for _, j := range jobs {
			_, err := r.client.BatchV1().Jobs(r.namespace).Get(ctx, j.Name, metav1.GetOptions{})
			if err == nil {
				allGone = false
				break
			}
			if !apierrors.IsNotFound(err) {
				allGone = false
				break
			}
			pods, err := r.client.CoreV1().Pods(r.namespace).List(ctx, metav1.ListOptions{
				LabelSelector: fmt.Sprintf("job-name=%s", j.Name),
			})
			if err != nil || len(pods.Items) > 0 {
				allGone = false
				break
			}
		}
		if allGone {
			return
		}
		time.Sleep(1 * time.Second)
	}
	fmt.Fprintf(r.output, "  Warning: some job pods still terminating after cleanup\n")
}
