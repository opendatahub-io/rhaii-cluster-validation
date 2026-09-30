package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/config"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const operatorNS = "openshift-sriov-network-operator"

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

// policy mirrors a SriovNetworkNodePolicy pinned to one node by hostname, as on
// clusters whose nodes carry different NIC layouts.
func policy(name, node, pool string, isRDMA bool, linkType string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"resourceName": pool,
			"isRdma":       isRDMA,
			"deviceType":   "netdevice",
			"linkType":     linkType,
			"nodeSelector": map[string]any{"kubernetes.io/hostname": node},
		},
	}}
	u.SetAPIVersion("sriovnetwork.openshift.io/v1")
	u.SetKind("SriovNetworkNodePolicy")
	u.SetNamespace(operatorNS)
	u.SetName(name)
	return u
}

func sriovNet(kind, namespace, name, pool, networkNamespace string) *unstructured.Unstructured {
	spec := map[string]any{"resourceName": pool}
	if networkNamespace != "" {
		spec["networkNamespace"] = networkNamespace
	}
	u := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	u.SetAPIVersion("sriovnetwork.openshift.io/v1")
	u.SetKind(kind)
	u.SetNamespace(namespace)
	u.SetName(name)
	return u
}

var gvrByKind = map[string]schema.GroupVersionResource{
	"NetworkAttachmentDefinition": nadGVR,
	"SriovNetworkNodePolicy":      sriovPolicyGVR,
	"SriovNetwork":                sriovNetworkGVR,
	"SriovIBNetwork":              sriovIBNetworkGVR,
	"SriovNetworkNodeState":       sriovNodeStateGVR,
}

// newFakeDynamic seeds objects through their real GVRs; the fake's default
// kind-to-resource guess does not match these CRD resource names.
func newFakeDynamic(t *testing.T, objs ...*unstructured.Unstructured) *dynamicfake.FakeDynamicClient {
	t.Helper()
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			nadGVR:            "NetworkAttachmentDefinitionList",
			sriovPolicyGVR:    "SriovNetworkNodePolicyList",
			sriovNetworkGVR:   "SriovNetworkList",
			sriovIBNetworkGVR: "SriovIBNetworkList",
			sriovNodeStateGVR: "SriovNetworkNodeStateList",
		})
	for _, obj := range objs {
		gvr := gvrByKind[obj.GetKind()]
		if _, err := client.Resource(gvr).Namespace(obj.GetNamespace()).
			Create(context.Background(), obj, metav1.CreateOptions{}); err != nil {
			t.Fatalf("failed to seed %s %s/%s: %v", obj.GetKind(), obj.GetNamespace(), obj.GetName(), err)
		}
	}
	return client
}

func multusConfig(isolated bool, globals string) *corev1.ConfigMap {
	iso := "false"
	if isolated {
		iso = "true"
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: multusConfigNamespace, Name: multusConfigMapName},
		Data:       map[string]string{multusConfigKey: `{"namespaceIsolation":` + iso + `,"globalNamespaces":"` + globals + `"}`},
	}
}

// sriovNode builds the node view discovery reads: hostname label plus allocatable.
func sriovNode(name string, allocatable map[string]int64) nodeInfo {
	res := corev1.ResourceList{}
	for k, v := range allocatable {
		res[corev1.ResourceName(k)] = *resource.NewQuantity(v, resource.DecimalSI)
	}
	return nodeInfo{labels: map[string]string{"kubernetes.io/hostname": name}, allocatable: res}
}

// pokprodLike is two GPU nodes sharing rail p6 through a SriovNetwork in a Multus global namespace.
func pokprodLike(t *testing.T, extra ...*unstructured.Unstructured) (*Controller, *strings.Builder) {
	t.Helper()
	c, _ := newTestController(fake.NewSimpleClientset(multusConfig(true, "default,"+operatorNS))) //nolint:staticcheck
	objs := []*unstructured.Unstructured{
		policy("p6-a", "node-a", "p6rdma", true, "eth"),
		policy("p6-b", "node-b", "p6rdma", true, "eth"),
		sriovNet("SriovNetwork", operatorNS, "roce-p6-shared", "p6rdma", operatorNS),
		nad(operatorNS, "roce-p6-shared", "openshift.io/p6rdma"),
	}
	c.dynamic = newFakeDynamic(t, append(objs, extra...)...)
	c.gpuNodes = []string{"node-a", "node-b"}
	c.gpuNodeInfo = map[string]nodeInfo{
		"node-a": sriovNode("node-a", map[string]int64{"openshift.io/p6rdma": 8, "openshift.io/p2rdma": 8}),
		"node-b": sriovNode("node-b", map[string]int64{"openshift.io/p6rdma": 8, "openshift.io/p2rdma": 8}),
	}
	out := &strings.Builder{}
	c.output = out
	return c, out
}

func plansOf(c *Controller) map[string]string {
	got := make(map[string]string, len(c.sriovRDMAPlans))
	for _, p := range c.sriovRDMAPlans {
		got[p.resource] = p.network
	}
	return got
}

func TestResolveSRIOVRDMAFromOperatorObjects(t *testing.T) {
	// A NAD nobody's SriovNetwork targets (left over by hand) must not matter.
	c, out := pokprodLike(t, nad("other-team", "storage-p0v0", "openshift.io/p0_storage"))

	c.resolveSRIOVRDMA(context.Background())

	want := map[string]string{"openshift.io/p6rdma": operatorNS + "/roce-p6-shared"}
	if got := plansOf(c); len(got) != 1 || got["openshift.io/p6rdma"] != want["openshift.io/p6rdma"] {
		t.Fatalf("plans = %v, want %v\n%s", got, want, out)
	}
	if !strings.Contains(out.String(), "auto-detected 1 rail(s)") {
		t.Errorf("detection not reported:\n%s", out)
	}
}

func TestResolveSRIOVRDMAIgnoresResourceNames(t *testing.T) {
	// The pool name proves nothing: a non-RDMA pool called *rdma is skipped and
	// an RDMA pool with another name is used.
	c, _ := pokprodLike(t,
		policy("fast-a", "node-a", "fastrdma", false, "eth"),
		policy("fast-b", "node-b", "fastrdma", false, "eth"),
		sriovNet("SriovNetwork", operatorNS, "fast", "fastrdma", operatorNS),
		nad(operatorNS, "fast", "openshift.io/fastrdma"),
		policy("vf-a", "node-a", "p8vf", true, "eth"),
		policy("vf-b", "node-b", "p8vf", true, "eth"),
		sriovNet("SriovNetwork", operatorNS, "roce-p8", "p8vf", operatorNS),
		nad(operatorNS, "roce-p8", "openshift.io/p8vf"),
	)
	for _, n := range []string{"node-a", "node-b"} {
		info := c.gpuNodeInfo[n]
		info.allocatable["openshift.io/fastrdma"] = *resource.NewQuantity(8, resource.DecimalSI)
		info.allocatable["openshift.io/p8vf"] = *resource.NewQuantity(8, resource.DecimalSI)
	}

	c.resolveSRIOVRDMA(context.Background())

	got := plansOf(c)
	if _, ok := got["openshift.io/fastrdma"]; ok {
		t.Errorf("non-RDMA pool fastrdma must not be attached: %v", got)
	}
	if got["openshift.io/p8vf"] != operatorNS+"/roce-p8" {
		t.Errorf("RDMA pool p8vf must be attached regardless of its name: %v", got)
	}
}

func TestResolveSRIOVRDMAUsesNADResourcePrefix(t *testing.T) {
	c, _ := newTestController(fake.NewSimpleClientset()) //nolint:staticcheck
	c.output = &strings.Builder{}
	c.dynamic = newFakeDynamic(t,
		policy("p6-a", "node-a", "p6rdma", true, "eth"),
		sriovNet("SriovNetwork", operatorNS, "roce-p6", "p6rdma", operatorNS),
		nad(operatorNS, "roce-p6", "example.com/p6rdma"),
	)
	c.gpuNodes = []string{"node-a"}
	c.gpuNodeInfo = map[string]nodeInfo{"node-a": sriovNode("node-a", map[string]int64{"example.com/p6rdma": 8})}

	c.resolveSRIOVRDMA(context.Background())

	if got := plansOf(c); got["example.com/p6rdma"] == "" {
		t.Fatalf("expected the NAD's full resource key to be used, got %v", got)
	}
}

func TestResolveSRIOVRDMAMultusIsolationPicksAttachableNetwork(t *testing.T) {
	// p2rdma has two networks; only the one in a global namespace is attachable.
	extra := []*unstructured.Unstructured{
		policy("p2-a", "node-a", "p2rdma", true, "eth"),
		policy("p2-b", "node-b", "p2rdma", true, "eth"),
		sriovNet("SriovNetwork", operatorNS, "roce-p2", "p2rdma", "autoscaling-example"),
		nad("autoscaling-example", "roce-p2", "openshift.io/p2rdma"),
		sriovNet("SriovNetwork", operatorNS, "roce-p2-shared", "p2rdma", operatorNS),
		nad(operatorNS, "roce-p2-shared", "openshift.io/p2rdma"),
	}

	t.Run("isolation known: unattachable network is skipped", func(t *testing.T) {
		c, out := pokprodLike(t, extra...)
		c.resolveSRIOVRDMA(context.Background())
		if got := plansOf(c)["openshift.io/p2rdma"]; got != operatorNS+"/roce-p2-shared" {
			t.Fatalf("p2rdma network = %q, want the global one\n%s", got, out)
		}
	})

	t.Run("isolation unknown: two candidates stay ambiguous", func(t *testing.T) {
		c, out := pokprodLike(t, extra...)
		c.client = fake.NewSimpleClientset() //nolint:staticcheck
		c.resolveSRIOVRDMA(context.Background())
		if _, ok := plansOf(c)["openshift.io/p2rdma"]; ok {
			t.Fatalf("ambiguous pool must not be guessed: %v", plansOf(c))
		}
		if !strings.Contains(out.String(), "matches several networks") {
			t.Errorf("ambiguity not reported:\n%s", out)
		}
	})
}

func TestResolveSRIOVRDMANamespacedNetwork(t *testing.T) {
	// No networkNamespace: the operator renders the NAD next to the network CR.
	extra := []*unstructured.Unstructured{
		policy("p2-a", "node-a", "p2rdma", true, "eth"),
		policy("p2-b", "node-b", "p2rdma", true, "eth"),
		sriovNet("SriovNetwork", "team-a", "roce-p2", "p2rdma", ""),
		nad("team-a", "roce-p2", "openshift.io/p2rdma"),
	}

	t.Run("run namespace differs: denied by isolation", func(t *testing.T) {
		c, out := pokprodLike(t, extra...)
		c.resolveSRIOVRDMA(context.Background())
		if _, ok := plansOf(c)["openshift.io/p2rdma"]; ok {
			t.Fatalf("NAD in team-a must not be attached from %s", c.opts.Namespace)
		}
		if !strings.Contains(out.String(), "not attachable from namespace") {
			t.Errorf("isolation reason not reported:\n%s", out)
		}
	})

	t.Run("run namespace matches: referenced by name", func(t *testing.T) {
		c, _ := pokprodLike(t, extra...)
		c.opts.Namespace = "team-a"
		c.resolveSRIOVRDMA(context.Background())
		if got := plansOf(c)["openshift.io/p2rdma"]; got != "roce-p2" {
			t.Fatalf("network ref = %q, want bare name in the run namespace", got)
		}
	})
}

func TestResolveSRIOVRDMASkipsPoolsNotUsableOnEveryNode(t *testing.T) {
	tests := []struct {
		name   string
		extra  []*unstructured.Unstructured
		tweak  func(c *Controller)
		reason string
	}{
		{
			name: "policies disagree on isRdma",
			extra: []*unstructured.Unstructured{
				policy("p2-a", "node-a", "p2rdma", true, "eth"),
				policy("p2-a-plain", "node-a", "p2rdma", false, "eth"),
				policy("p2-b", "node-b", "p2rdma", true, "eth"),
				sriovNet("SriovNetwork", operatorNS, "roce-p2", "p2rdma", operatorNS),
				nad(operatorNS, "roce-p2", "openshift.io/p2rdma"),
			},
			reason: "non-RDMA policies on node-a (p2-a-plain)",
		},
		{
			name: "pool configured on one node only",
			extra: []*unstructured.Unstructured{
				policy("p2-a", "node-a", "p2rdma", true, "eth"),
				sriovNet("SriovNetwork", operatorNS, "roce-p2", "p2rdma", operatorNS),
				nad(operatorNS, "roce-p2", "openshift.io/p2rdma"),
			},
			reason: "not configured on node-b",
		},
		{
			name: "policy applied but node advertises no VFs",
			extra: []*unstructured.Unstructured{
				policy("p2-a", "node-a", "p2rdma", true, "eth"),
				policy("p2-b", "node-b", "p2rdma", true, "eth"),
				sriovNet("SriovNetwork", operatorNS, "roce-p2", "p2rdma", operatorNS),
				nad(operatorNS, "roce-p2", "openshift.io/p2rdma"),
			},
			tweak: func(c *Controller) {
				c.gpuNodeInfo["node-b"].allocatable["openshift.io/p2rdma"] = *resource.NewQuantity(0, resource.DecimalSI)
			},
			reason: "no allocatable openshift.io/p2rdma on node-b",
		},
		{
			name: "SriovNetwork exists but its NAD is missing",
			extra: []*unstructured.Unstructured{
				policy("p2-a", "node-a", "p2rdma", true, "eth"),
				policy("p2-b", "node-b", "p2rdma", true, "eth"),
				sriovNet("SriovNetwork", operatorNS, "roce-p2", "p2rdma", operatorNS),
			},
			reason: "NAD " + operatorNS + "/roce-p2 not readable",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, out := pokprodLike(t, tt.extra...)
			if tt.tweak != nil {
				tt.tweak(c)
			}
			c.resolveSRIOVRDMA(context.Background())
			if _, ok := plansOf(c)["openshift.io/p2rdma"]; ok {
				t.Fatalf("p2rdma must be skipped: %v", plansOf(c))
			}
			if plansOf(c)["openshift.io/p6rdma"] == "" {
				t.Errorf("the healthy p6 rail must still be attached: %v", plansOf(c))
			}
			if !strings.Contains(out.String(), tt.reason) {
				t.Errorf("expected reason %q in:\n%s", tt.reason, out)
			}
		})
	}
}

func TestResolveSRIOVRDMAInfiniBand(t *testing.T) {
	setup := func(t *testing.T, networkKind string) (*Controller, *strings.Builder) {
		c, _ := newTestController(fake.NewSimpleClientset()) //nolint:staticcheck
		out := &strings.Builder{}
		c.output = out
		c.dynamic = newFakeDynamic(t,
			policy("ib0-a", "node-a", "ib0rdma", true, "ib"),
			sriovNet(networkKind, operatorNS, "ib0", "ib0rdma", operatorNS),
			nad(operatorNS, "ib0", "openshift.io/ib0rdma"),
		)
		c.gpuNodes = []string{"node-a"}
		c.gpuNodeInfo = map[string]nodeInfo{"node-a": sriovNode("node-a", map[string]int64{"openshift.io/ib0rdma": 8})}
		return c, out
	}

	t.Run("SriovIBNetwork serves an IB pool", func(t *testing.T) {
		c, out := setup(t, "SriovIBNetwork")
		c.resolveSRIOVRDMA(context.Background())
		if plansOf(c)["openshift.io/ib0rdma"] == "" {
			t.Fatalf("IB rail not detected:\n%s", out)
		}
	})

	t.Run("Ethernet SriovNetwork does not serve an IB pool", func(t *testing.T) {
		c, out := setup(t, "SriovNetwork")
		c.resolveSRIOVRDMA(context.Background())
		if len(c.sriovRDMAPlans) != 0 {
			t.Fatalf("kind mismatch must be skipped: %v", plansOf(c))
		}
		if !strings.Contains(out.String(), "does not match the pool's link type") {
			t.Errorf("mismatch not reported:\n%s", out)
		}
	})
}

func TestResolveSRIOVRDMAMissingOrForbiddenCRDs(t *testing.T) {
	t.Run("no SR-IOV operator: silent, no plans", func(t *testing.T) {
		c, out := pokprodLike(t)
		dyn := c.dynamic.(*dynamicfake.FakeDynamicClient)
		dyn.PrependReactor("list", "sriovnetworknodepolicies", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewNotFound(sriovPolicyGVR.GroupResource(), "")
		})
		c.resolveSRIOVRDMA(context.Background())
		if len(c.sriovRDMAPlans) != 0 || out.Len() != 0 {
			t.Fatalf("expected silent skip, got plans %v output %q", plansOf(c), out)
		}
	})

	t.Run("policies forbidden: points to manual config", func(t *testing.T) {
		c, out := pokprodLike(t)
		dyn := c.dynamic.(*dynamicfake.FakeDynamicClient)
		dyn.PrependReactor("list", "sriovnetworknodepolicies", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(sriovPolicyGVR.GroupResource(), "", nil)
		})
		c.resolveSRIOVRDMA(context.Background())
		if len(c.sriovRDMAPlans) != 0 || !strings.Contains(out.String(), "configure SR-IOV RDMA manually") {
			t.Fatalf("expected manual-config hint, got plans %v output %q", plansOf(c), out)
		}
	})

	t.Run("IB network CRD absent: Ethernet rails still work", func(t *testing.T) {
		c, _ := pokprodLike(t)
		dyn := c.dynamic.(*dynamicfake.FakeDynamicClient)
		dyn.PrependReactor("list", "sriovibnetworks", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewNotFound(sriovIBNetworkGVR.GroupResource(), "")
		})
		c.resolveSRIOVRDMA(context.Background())
		if plansOf(c)["openshift.io/p6rdma"] == "" {
			t.Fatalf("missing IB CRD must not disable Ethernet detection: %v", plansOf(c))
		}
	})
}

func TestResolveSRIOVRDMAManualConfigWins(t *testing.T) {
	for _, jobs := range []config.ResourceConfig{
		{Requests: map[string]string{"rdma/ib": "1"}},
		{Annotations: map[string]string{config.MultusNetworksAnnotation: "team/roce"}},
	} {
		c, out := pokprodLike(t)
		c.resolveSRIOVRDMA(context.Background()) // detected first...
		c.cfg.Jobs = jobs
		c.resolveSRIOVRDMA(context.Background()) // ...then manual config appears
		if c.sriovRDMAPlans != nil {
			t.Fatalf("manual config %+v must clear auto-detected plans, got %v\n%s", jobs, plansOf(c), out)
		}
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
