package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/config"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func nad(namespace, name, resourceName string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("k8s.cni.cncf.io/v1")
	u.SetKind("NetworkAttachmentDefinition")
	u.SetNamespace(namespace)
	u.SetName(name)
	if resourceName != "" {
		u.SetAnnotations(map[string]string{config.NADResourceAnnotation: resourceName})
	}
	return u
}

// newFakeDynamic returns a dynamic client preloaded with NADs. Objects are
// created through the client so they land on nadGVR: the fake's default
// kind-to-resource guess ("networkattachmentdefinitions") does not match the
// real resource name.
func newFakeDynamic(t *testing.T, objs ...*unstructured.Unstructured) *dynamicfake.FakeDynamicClient {
	t.Helper()
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{nadGVR: "NetworkAttachmentDefinitionList"},
	)
	for _, obj := range objs {
		if _, err := client.Resource(nadGVR).Namespace(obj.GetNamespace()).
			Create(context.Background(), obj, metav1.CreateOptions{}); err != nil {
			t.Fatalf("failed to seed NAD %s/%s: %v", obj.GetNamespace(), obj.GetName(), err)
		}
	}
	return client
}

func TestResolveSRIOVRDMAMatchesRailsInRunNamespace(t *testing.T) {
	c, buf := newTestController(nil)
	c.dynamic = newFakeDynamic(t,
		nad("test-ns", "roce-p0", "openshift.io/p0rdma"),
		nad("test-ns", "roce-p4", "openshift.io/p4rdma"),
	)
	c.gpuNodes = []string{"node-a", "node-b"}
	c.sriovRDMAResources = map[string][]string{
		"node-a": {"openshift.io/p0rdma", "openshift.io/p4rdma"},
		"node-b": {"openshift.io/p0rdma", "openshift.io/p4rdma"},
	}

	c.resolveSRIOVRDMA(context.Background())

	if len(c.sriovRDMAPlans) != 2 {
		t.Fatalf("expected 2 resolved rails, got %d (%v)", len(c.sriovRDMAPlans), c.sriovRDMAPlans)
	}
	if c.sriovRDMAPlans[0].resource != "openshift.io/p0rdma" || c.sriovRDMAPlans[0].network != "roce-p0" {
		t.Errorf("unexpected first plan: %+v", c.sriovRDMAPlans[0])
	}
	if !strings.Contains(buf.String(), "auto-detected 2 rail(s)") {
		t.Errorf("expected detection to be reported, got:\n%s", buf.String())
	}
}

func TestResolveSRIOVRDMAExcludesRailsMissingOnSomeNodes(t *testing.T) {
	c, _ := newTestController(nil)
	c.dynamic = newFakeDynamic(t,
		nad("test-ns", "roce-p4", "openshift.io/p4rdma"),
		nad("test-ns", "roce-p10", "openshift.io/p10rdma"),
	)
	c.gpuNodes = []string{"node-6-rail", "node-8-rail"}
	c.sriovRDMAResources = map[string][]string{
		"node-6-rail": {"openshift.io/p4rdma"},
		"node-8-rail": {"openshift.io/p4rdma", "openshift.io/p10rdma"},
	}

	c.resolveSRIOVRDMA(context.Background())

	if len(c.sriovRDMAPlans) != 1 || c.sriovRDMAPlans[0].resource != "openshift.io/p4rdma" {
		t.Fatalf("expected only the shared rail, got %+v", c.sriovRDMAPlans)
	}
}

func TestResolveSRIOVRDMAIgnoresUnselectedNodes(t *testing.T) {
	c, _ := newTestController(nil)
	c.dynamic = newFakeDynamic(t, nad("test-ns", "roce-p4", "openshift.io/p4rdma"))
	c.gpuNodes = []string{"selected"}
	c.sriovRDMAResources = map[string][]string{
		"selected":     {"openshift.io/p4rdma"},
		"not-selected": nil,
	}

	c.resolveSRIOVRDMA(context.Background())

	if len(c.sriovRDMAPlans) != 1 {
		t.Fatalf("expected the selected node's rail to resolve, got %+v", c.sriovRDMAPlans)
	}
}

func TestResolveSRIOVRDMAConfigOverrideWins(t *testing.T) {
	c, _ := newTestController(nil)
	c.dynamic = newFakeDynamic(t, nad("test-ns", "roce-p4", "openshift.io/p4rdma"))
	c.gpuNodes = []string{"node-a"}
	c.sriovRDMAResources = map[string][]string{"node-a": {"openshift.io/p4rdma"}}
	c.cfg.Jobs.Requests = map[string]string{"openshift.io/p2rdma": "1"}

	c.resolveSRIOVRDMA(context.Background())

	if c.sriovRDMAPlans != nil {
		t.Fatalf("auto-detection must not override explicit config, got %+v", c.sriovRDMAPlans)
	}
}

func TestResolveSRIOVRDMAReportsUnmatchedRail(t *testing.T) {
	c, buf := newTestController(nil)
	c.dynamic = newFakeDynamic(t) // no NADs at all
	c.gpuNodes = []string{"node-a"}
	c.sriovRDMAResources = map[string][]string{"node-a": {"openshift.io/p4rdma"}}

	c.resolveSRIOVRDMA(context.Background())

	if c.sriovRDMAPlans != nil {
		t.Fatalf("expected no plans without a NAD, got %+v", c.sriovRDMAPlans)
	}
	out := buf.String()
	if !strings.Contains(out, "no NetworkAttachmentDefinition") || !strings.Contains(out, "openshift.io/p4rdma") {
		t.Errorf("expected an actionable message naming the rail, got:\n%s", out)
	}
}

func TestResolveSRIOVRDMAAmbiguousMatchIsNotGuessed(t *testing.T) {
	c, buf := newTestController(nil)
	c.dynamic = newFakeDynamic(t,
		nad("test-ns", "roce-p4-a", "openshift.io/p4rdma"),
		nad("test-ns", "roce-p4-b", "openshift.io/p4rdma"),
	)
	c.gpuNodes = []string{"node-a"}
	c.sriovRDMAResources = map[string][]string{"node-a": {"openshift.io/p4rdma"}}

	c.resolveSRIOVRDMA(context.Background())

	if c.sriovRDMAPlans != nil {
		t.Fatalf("ambiguous match must not be guessed, got %+v", c.sriovRDMAPlans)
	}
	if !strings.Contains(buf.String(), "multiple NetworkAttachmentDefinitions") {
		t.Errorf("expected ambiguity to be reported, got:\n%s", buf.String())
	}
}

func TestResolveSRIOVRDMACrossNamespaceRequiresOptIn(t *testing.T) {
	objs := []*unstructured.Unstructured{nad("sriov-ns", "roce-p4-shared", "openshift.io/p4rdma")}

	c, _ := newTestController(nil)
	c.dynamic = newFakeDynamic(t, objs...)
	c.gpuNodes = []string{"node-a"}
	c.sriovRDMAResources = map[string][]string{"node-a": {"openshift.io/p4rdma"}}

	c.resolveSRIOVRDMA(context.Background())
	if c.sriovRDMAPlans != nil {
		t.Fatalf("must not reach into another namespace without opt-in, got %+v", c.sriovRDMAPlans)
	}

	optedIn, _ := newTestController(nil)
	optedIn.dynamic = newFakeDynamic(t, objs...)
	optedIn.gpuNodes = []string{"node-a"}
	optedIn.sriovRDMAResources = map[string][]string{"node-a": {"openshift.io/p4rdma"}}
	optedIn.cfg.Jobs.SRIOVRDMANADNamespace = "sriov-ns"

	optedIn.resolveSRIOVRDMA(context.Background())
	if len(optedIn.sriovRDMAPlans) != 1 {
		t.Fatalf("expected opt-in to resolve the shared NAD, got %+v", optedIn.sriovRDMAPlans)
	}
	if got := optedIn.sriovRDMAPlans[0].network; got != "sriov-ns/roce-p4-shared" {
		t.Errorf("cross-namespace reference = %q, want namespace-qualified", got)
	}
}

func TestApplySRIOVRDMAInjectsResourcesAndAttachment(t *testing.T) {
	c, _ := newTestController(nil)
	c.sriovRDMAPlans = []sriovRDMAPlan{
		{resource: "openshift.io/p0rdma", network: "roce-p0"},
		{resource: "openshift.io/p4rdma", network: "sriov-ns/roce-p4-shared"},
	}

	container := &corev1.Container{}
	annotations := map[string]string{}
	applied := c.applySRIOVRDMA(container, annotations)

	if len(applied) != 2 {
		t.Fatalf("expected 2 applied resources, got %v", applied)
	}
	for _, res := range []corev1.ResourceName{"openshift.io/p0rdma", "openshift.io/p4rdma"} {
		if qty, ok := container.Resources.Requests[res]; !ok || qty.Value() != 1 {
			t.Errorf("request for %s = %v, want 1", res, qty)
		}
		if qty, ok := container.Resources.Limits[res]; !ok || qty.Value() != 1 {
			t.Errorf("limit for %s = %v, want 1", res, qty)
		}
	}
	want := "roce-p0,sriov-ns/roce-p4-shared"
	if got := annotations[config.MultusNetworksAnnotation]; got != want {
		t.Errorf("attachment annotation = %q, want %q", got, want)
	}
}

func TestApplySRIOVRDMANoPlansLeavesPodUntouched(t *testing.T) {
	c, _ := newTestController(nil)
	container := &corev1.Container{}
	annotations := map[string]string{}

	if applied := c.applySRIOVRDMA(container, annotations); applied != nil {
		t.Errorf("expected no resources applied, got %v", applied)
	}
	if len(container.Resources.Requests) != 0 || len(annotations) != 0 {
		t.Errorf("non-SR-IOV clusters must be untouched: %v / %v", container.Resources, annotations)
	}
}
