package operator

import (
	"context"
	"fmt"
	"strings"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// OperatorSpec defines an operator to check for pod health.
type OperatorSpec struct {
	// Identifier used for config overrides, e.g. "cert-manager"
	Name string
	// Human-readable description for the report
	Description string
	// Namespaces to check for running pods (platform-dependent defaults)
	Namespaces []string
	// Remediation hint shown when the operator is not found
	Remediation string
	// Optional marks the operator as not strictly required: when it is missing
	// or unhealthy the check reports WARN instead of FAIL, so a cluster is not
	// marked NOT READY on its account.
	Optional bool
}

// RequiredOperators lists the operators required for llm-d deployment.
//
// TODO: Add KServe controller check. The namespace varies by platform:
//   - OCP with RHOAI: "redhat-ods-applications" vs "opendatahub"
var RequiredOperators = []OperatorSpec{
	{
		Name:        "cert-manager",
		Description: "cert-manager",
		Namespaces:  []string{"cert-manager"},
		Remediation: "Install cert-manager: kubectl apply -f https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml",
	},
	{
		Name:        "istio",
		Description: "Istio service mesh",
		Namespaces:  []string{"istio-system"},
		Remediation: "Install Istio: https://istio.io/latest/docs/setup/install/",
	},
	{
		Name:        "lws",
		Description: "LeaderWorkerSet operator",
		Namespaces:  []string{"openshift-lws-operator"},
		Remediation: "Install LeaderWorkerSet: https://github.com/kubernetes-sigs/lws#installation",
		Optional:    true,
	},
}

// Checker validates that required operators have healthy pods.
type Checker struct {
	client    kubernetes.Interface
	operators []OperatorSpec
}

// NewChecker creates an operator health checker.
// nsOverrides maps operator name to namespace list, overriding defaults from platform config.
func NewChecker(client kubernetes.Interface, operators []OperatorSpec, nsOverrides map[string][]string) *Checker {
	if len(operators) == 0 {
		operators = make([]OperatorSpec, len(RequiredOperators))
		copy(operators, RequiredOperators)
	}
	for i := range operators {
		if ns, ok := nsOverrides[operators[i].Name]; ok && len(ns) > 0 {
			operators[i].Namespaces = ns
		}
	}
	return &Checker{client: client, operators: operators}
}

// Run checks each operator and returns the results.
func (c *Checker) Run(ctx context.Context) []checks.Result {
	results := make([]checks.Result, 0, len(c.operators))
	for _, spec := range c.operators {
		results = append(results, c.checkOperator(ctx, spec))
	}
	return results
}

// checkOperator verifies that at least one of the operator's expected namespaces
// exists and contains healthy pods.
func (c *Checker) checkOperator(ctx context.Context, spec OperatorSpec) checks.Result {
	r := checks.Result{
		Category: "operator",
		Name:     spec.Name,
	}

	anyNamespaceFound := false
	totalRunning := 0
	totalFailed := 0
	checkedNamespaces := []string{}

	for _, ns := range spec.Namespaces {
		_, err := c.client.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			r.Status = checks.StatusWarn
			r.Message = fmt.Sprintf("%s: failed to check namespace %s: %v", spec.Description, ns, err)
			return r
		}

		anyNamespaceFound = true
		checkedNamespaces = append(checkedNamespaces, ns)

		pods, err := c.client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			r.Status = checks.StatusWarn
			r.Message = fmt.Sprintf("%s: failed to list pods in %s: %v", spec.Description, ns, err)
			return r
		}

		for _, pod := range pods.Items {
			if isFailed(pod) {
				totalFailed++
			} else if pod.Status.Phase == corev1.PodRunning {
				totalRunning++
			}
		}
	}

	if !anyNamespaceFound {
		r.Status = missingStatus(spec)
		r.Message = fmt.Sprintf("%s: not installed (checked namespaces: %s)", spec.Description, strings.Join(spec.Namespaces, ", "))
		r.Remediation = spec.Remediation
		return r
	}

	if totalFailed > 0 {
		r.Status = missingStatus(spec)
		r.Message = fmt.Sprintf("%s: %d pod(s) failing in %s", spec.Description, totalFailed, strings.Join(checkedNamespaces, ", "))
		return r
	}

	if totalRunning > 0 {
		r.Status = checks.StatusPass
		r.Message = fmt.Sprintf("%s: %d pod(s) running in %s", spec.Description, totalRunning, strings.Join(checkedNamespaces, ", "))
		return r
	}

	r.Status = missingStatus(spec)
	r.Message = fmt.Sprintf("%s: namespace(s) exist but no running pods found in %s", spec.Description, strings.Join(checkedNamespaces, ", "))
	r.Remediation = spec.Remediation
	return r
}

// missingStatus returns WARN for optional operators and FAIL otherwise, so an
// optional operator that is missing or unhealthy never marks the cluster as
// NOT READY.
func missingStatus(spec OperatorSpec) checks.Status {
	if spec.Optional {
		return checks.StatusWarn
	}
	return checks.StatusFail
}

func isFailed(pod corev1.Pod) bool {
	if pod.Status.Phase == corev1.PodFailed {
		return true
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Waiting != nil {
			reason := cs.State.Waiting.Reason
			if reason == "CrashLoopBackOff" || reason == "Error" || reason == "ImagePullBackOff" || reason == "ErrImagePull" {
				return true
			}
		}
	}
	return false
}
