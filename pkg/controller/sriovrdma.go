package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/config"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	nadGVR = schema.GroupVersionResource{
		Group:    "k8s.cni.cncf.io",
		Version:  "v1",
		Resource: "network-attachment-definitions",
	}
	sriovPolicyGVR = schema.GroupVersionResource{
		Group:    "sriovnetwork.openshift.io",
		Version:  "v1",
		Resource: "sriovnetworknodepolicies",
	}
	sriovNetworkGVR = schema.GroupVersionResource{
		Group:    "sriovnetwork.openshift.io",
		Version:  "v1",
		Resource: "sriovnetworks",
	}
	sriovIBNetworkGVR = schema.GroupVersionResource{
		Group:    "sriovnetwork.openshift.io",
		Version:  "v1",
		Resource: "sriovibnetworks",
	}
	sriovNodeStateGVR = schema.GroupVersionResource{
		Group:    "sriovnetwork.openshift.io",
		Version:  "v1",
		Resource: "sriovnetworknodestates",
	}
)

// OpenShift keeps the Multus namespace-isolation settings in this ConfigMap.
const (
	multusConfigNamespace = "openshift-multus"
	multusConfigMapName   = "multus-daemon-config"
	multusConfigKey       = "daemon-config.json"
)

// nodeInfo is the part of a GPU node that SR-IOV discovery needs.
type nodeInfo struct {
	labels      map[string]string
	allocatable corev1.ResourceList
}

type sriovRDMAPlan struct {
	resource string // full device-plugin resource, e.g. openshift.io/p6rdma
	network  string // Multus reference: name (run namespace) or namespace/name
}

// rdmaPool is an SR-IOV resource pool that is RDMA-enabled on every selected node.
type rdmaPool struct {
	name       string // policy resourceName, without prefix
	infiniband bool
}

// sriovNetworkRef is a SriovNetwork or SriovIBNetwork and the NAD the operator renders for it.
type sriovNetworkRef struct {
	kind       string
	namespace  string
	name       string
	resource   string // spec.resourceName
	targetNS   string // where the NAD lives
	infiniband bool
}

// resolveSRIOVRDMA plans one (resource, NAD) pair per RDMA rail proven by the SR-IOV operator CRs,
// node allocatable, and Multus isolation; unproven rails are skipped with a reason.
func (c *Controller) resolveSRIOVRDMA(ctx context.Context) {
	c.sriovRDMAPlans = nil
	c.sriovResolved = true
	if c.dynamic == nil || config.ResourceConfigOwnsRDMAAttachment(c.cfg.Jobs) {
		return
	}

	policies, err := c.dynamic.Resource(sriovPolicyGVR).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			c.sriovNote("skipping auto-detection, cannot list SriovNetworkNodePolicies: %v. "+
				"Set jobs.requests and jobs.annotations to configure SR-IOV RDMA manually", err)
		}
		return
	}

	pools, reasons := rdmaPoolsOnAllNodes(policies.Items, c.selectedNodeInfo())
	if len(pools) == 0 {
		c.sriovNotes(reasons)
		return
	}

	networks, notes := c.listSRIOVNetworks(ctx)
	reasons = append(reasons, notes...)
	reach := c.multusIsolation(ctx)

	var plans []sriovRDMAPlan
	for _, pool := range pools {
		plan, reason := c.planForPool(ctx, pool, networks, reach)
		if reason != "" {
			reasons = append(reasons, reason)
			continue
		}
		plans = append(plans, plan)
	}
	c.sriovNotes(reasons)
	if len(plans) == 0 {
		return
	}

	c.sriovRDMAPlans = plans
	resources := make([]string, 0, len(plans))
	networkRefs := make([]string, 0, len(plans))
	for _, p := range plans {
		resources = append(resources, p.resource)
		networkRefs = append(networkRefs, p.network)
	}
	fmt.Fprintf(c.output, "  SR-IOV RDMA: auto-detected %d rail(s): %s\n", len(plans), strings.Join(resources, ", "))
	fmt.Fprintf(c.output, "  SR-IOV RDMA: attaching %s\n", strings.Join(networkRefs, ", "))
}

func (c *Controller) selectedNodeInfo() map[string]nodeInfo {
	selected := make(map[string]nodeInfo, len(c.gpuNodes))
	for _, node := range c.gpuNodes {
		selected[node] = c.gpuNodeInfo[node]
	}
	return selected
}

// poolState aggregates every policy that feeds one pool on one node.
type poolState struct {
	rdma     bool // every contributing policy is RDMA-capable netdevice
	anyRDMA  bool
	ib       bool
	blockers []string
}

// rdmaPoolsOnAllNodes returns pools every selected node configures with only RDMA netdevice
// policies; the operator takes isRdma from one of several same-name policies, so disagreement is ambiguous.
func rdmaPoolsOnAllNodes(policies []unstructured.Unstructured, nodes map[string]nodeInfo) ([]rdmaPool, []string) {
	if len(nodes) == 0 {
		return nil, nil
	}
	perNode := make(map[string]map[string]*poolState, len(nodes))
	for node, info := range nodes {
		states := make(map[string]*poolState)
		for _, p := range policies {
			selector, _, _ := unstructured.NestedStringMap(p.Object, "spec", "nodeSelector")
			if !labelsMatch(selector, info.labels) {
				continue
			}
			pool, _, _ := unstructured.NestedString(p.Object, "spec", "resourceName")
			if pool == "" {
				continue
			}
			isRDMA, _, _ := unstructured.NestedBool(p.Object, "spec", "isRdma")
			deviceType, _, _ := unstructured.NestedString(p.Object, "spec", "deviceType")
			vdpaType, _, _ := unstructured.NestedString(p.Object, "spec", "vdpaType")
			linkType, _, _ := unstructured.NestedString(p.Object, "spec", "linkType")

			st, ok := states[pool]
			if !ok {
				st = &poolState{rdma: true}
				states[pool] = st
			}
			usable := isRDMA && (deviceType == "" || deviceType == "netdevice") && vdpaType == ""
			if usable {
				st.anyRDMA = true
			} else {
				st.rdma = false
				st.blockers = append(st.blockers, p.GetName())
			}
			st.ib = st.ib || strings.EqualFold(linkType, "ib")
		}
		perNode[node] = states
	}

	names := make(map[string]bool)
	for _, states := range perNode {
		for pool := range states {
			names[pool] = true
		}
	}

	var pools []rdmaPool
	var reasons []string
	for _, pool := range slices.Sorted(maps.Keys(names)) {
		var missing, conflicting []string
		anyRDMA, ib := false, false
		for _, node := range slices.Sorted(maps.Keys(nodes)) {
			st, ok := perNode[node][pool]
			if !ok {
				missing = append(missing, node)
				continue
			}
			anyRDMA = anyRDMA || st.anyRDMA
			ib = ib || st.ib
			if !st.rdma {
				conflicting = append(conflicting, fmt.Sprintf("%s (%s)", node, strings.Join(st.blockers, ", ")))
			}
		}
		if !anyRDMA {
			continue // an ordinary non-RDMA pool, nothing to report
		}
		switch {
		case len(missing) > 0:
			reasons = append(reasons, fmt.Sprintf("pool %s is not configured on %s", pool, strings.Join(missing, ", ")))
		case len(conflicting) > 0:
			reasons = append(reasons, fmt.Sprintf("pool %s has non-RDMA policies on %s", pool, strings.Join(conflicting, "; ")))
		default:
			pools = append(pools, rdmaPool{name: pool, infiniband: ib})
		}
	}
	return pools, reasons
}

// listSRIOVNetworks lists SriovNetworks and SriovIBNetworks cluster-wide. Each
// kind is optional: a cluster without the IB CRD still gets Ethernet rails.
func (c *Controller) listSRIOVNetworks(ctx context.Context) ([]sriovNetworkRef, []string) {
	kinds := []struct {
		gvr        schema.GroupVersionResource
		kind       string
		infiniband bool
	}{
		{sriovNetworkGVR, "SriovNetwork", false},
		{sriovIBNetworkGVR, "SriovIBNetwork", true},
	}
	var refs []sriovNetworkRef
	var notes []string
	for _, k := range kinds {
		list, err := c.dynamic.Resource(k.gvr).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
		if err != nil {
			if !apierrors.IsNotFound(err) {
				notes = append(notes, fmt.Sprintf("cannot list %ss: %v", k.kind, err))
			}
			continue
		}
		for _, item := range list.Items {
			resource, _, _ := unstructured.NestedString(item.Object, "spec", "resourceName")
			target, _, _ := unstructured.NestedString(item.Object, "spec", "networkNamespace")
			if target == "" {
				// The operator renders the NAD next to the network CR when no
				// networkNamespace is set (namespaced SriovNetwork).
				target = item.GetNamespace()
			}
			refs = append(refs, sriovNetworkRef{
				kind:       k.kind,
				namespace:  item.GetNamespace(),
				name:       item.GetName(),
				resource:   resource,
				targetNS:   target,
				infiniband: k.infiniband,
			})
		}
	}
	return refs, notes
}

// planForPool picks the single usable NAD for a pool, or returns why it cannot.
func (c *Controller) planForPool(ctx context.Context, pool rdmaPool, networks []sriovNetworkRef, reach multusIsolation) (sriovRDMAPlan, string) {
	var usable []sriovRDMAPlan
	var why []string
	for _, n := range networks {
		if n.resource != pool.name {
			continue
		}
		ref := n.targetNS + "/" + n.name
		if n.infiniband != pool.infiniband {
			why = append(why, fmt.Sprintf("%s %s/%s does not match the pool's link type", n.kind, n.namespace, n.name))
			continue
		}
		nadObj, err := c.dynamic.Resource(nadGVR).Namespace(n.targetNS).Get(ctx, n.name, metav1.GetOptions{})
		if err != nil {
			why = append(why, fmt.Sprintf("NAD %s not readable: %v", ref, err))
			continue
		}
		key := nadObj.GetAnnotations()[config.NADResourceAnnotation]
		if !strings.HasSuffix(key, "/"+pool.name) {
			why = append(why, fmt.Sprintf("NAD %s has resourceName %q, expected <prefix>/%s", ref, key, pool.name))
			continue
		}
		if reach.denies(n.targetNS, c.opts.Namespace) {
			why = append(why, fmt.Sprintf("NAD %s is not attachable from namespace %s (Multus namespace isolation)", ref, c.opts.Namespace))
			continue
		}
		network := ref
		if n.targetNS == c.opts.Namespace {
			network = n.name
		}
		usable = append(usable, sriovRDMAPlan{resource: key, network: network})
	}

	switch len(usable) {
	case 0:
		if len(why) == 0 {
			return sriovRDMAPlan{}, fmt.Sprintf("pool %s has no SriovNetwork", pool.name)
		}
		return sriovRDMAPlan{}, fmt.Sprintf("pool %s has no usable network: %s", pool.name, strings.Join(why, "; "))
	case 1:
	default:
		refs := make([]string, 0, len(usable))
		for _, u := range usable {
			refs = append(refs, u.network)
		}
		sort.Strings(refs)
		return sriovRDMAPlan{}, fmt.Sprintf("pool %s matches several networks (%s); set jobs.requests and jobs.annotations to choose",
			pool.name, strings.Join(refs, ", "))
	}

	plan := usable[0]
	var empty []string
	for node, info := range c.selectedNodeInfo() {
		if qty, ok := info.allocatable[corev1.ResourceName(plan.resource)]; !ok || qty.Value() <= 0 {
			empty = append(empty, node)
		}
	}
	if len(empty) > 0 {
		sort.Strings(empty)
		return sriovRDMAPlan{}, fmt.Sprintf("pool %s has no allocatable %s on %s", pool.name, plan.resource, strings.Join(empty, ", "))
	}
	return plan, ""
}

// multusIsolation is OpenShift's Multus namespace isolation; unreadable means unknown,
// so nothing is denied and the pod reports its own attach error.
type multusIsolation struct {
	known    bool
	isolated bool
	globals  map[string]bool
}

func (m multusIsolation) denies(nadNamespace, podNamespace string) bool {
	if !m.known || !m.isolated || nadNamespace == podNamespace {
		return false
	}
	return !m.globals[nadNamespace]
}

func (c *Controller) multusIsolation(ctx context.Context) multusIsolation {
	cm, err := c.client.CoreV1().ConfigMaps(multusConfigNamespace).Get(ctx, multusConfigMapName, metav1.GetOptions{})
	if err != nil {
		return multusIsolation{}
	}
	raw, ok := cm.Data[multusConfigKey]
	if !ok {
		return multusIsolation{}
	}
	var cfg struct {
		NamespaceIsolation bool   `json:"namespaceIsolation"`
		GlobalNamespaces   string `json:"globalNamespaces"`
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return multusIsolation{}
	}
	globals := make(map[string]bool)
	for _, ns := range strings.Split(cfg.GlobalNamespaces, ",") {
		if ns = strings.TrimSpace(ns); ns != "" {
			globals[ns] = true
		}
	}
	return multusIsolation{known: true, isolated: cfg.NamespaceIsolation, globals: globals}
}

func (c *Controller) sriovNote(format string, args ...any) {
	fmt.Fprintf(c.output, "  SR-IOV RDMA: "+format+"\n", args...)
}

func (c *Controller) sriovNotes(notes []string) {
	for _, n := range notes {
		c.sriovNote("%s", n)
	}
}

func labelsMatch(selector, labels map[string]string) bool {
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}

func (c *Controller) applySRIOVRDMA(container *corev1.Container, podAnnotations map[string]string) []string {
	if len(c.sriovRDMAPlans) == 0 {
		return nil
	}
	resources := make([]string, 0, len(c.sriovRDMAPlans))
	for _, p := range c.sriovRDMAPlans {
		setContainerResource(container, corev1.ResourceName(p.resource), 1)
		resources = append(resources, p.resource)
	}
	podAnnotations[config.MultusNetworksAnnotation] = c.sriovNetworks()
	return resources
}

func (c *Controller) sriovNetworks() string {
	networks := make([]string, 0, len(c.sriovRDMAPlans))
	for _, p := range c.sriovRDMAPlans {
		networks = append(networks, p.network)
	}
	return strings.Join(networks, ",")
}

// sriovNodeLayout is what one SriovNetworkNodeState says about planned rails:
// which PFs carry each rail, and which PF each VF belongs to.
type sriovNodeLayout struct {
	railPFs map[string][]string // rail resource -> PF PCI addresses
	vfPF    map[string]string   // VF PCI address -> PF PCI address
}

// sriovNodeLayouts reads SriovNetworkNodeStates for the selected nodes; nil when unreadable.
func (c *Controller) sriovNodeLayouts(ctx context.Context) map[string]sriovNodeLayout {
	if c.dynamic == nil || len(c.sriovRDMAPlans) == 0 {
		return nil
	}
	states, err := c.dynamic.Resource(sriovNodeStateGVR).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil
	}
	rails := make(map[string]string, len(c.sriovRDMAPlans)) // pool name -> rail resource
	for _, p := range c.sriovRDMAPlans {
		rails[p.resource[strings.LastIndex(p.resource, "/")+1:]] = p.resource
	}
	layouts := make(map[string]sriovNodeLayout)
	for _, s := range states.Items {
		if !slices.Contains(c.gpuNodes, s.GetName()) {
			continue
		}
		l := sriovNodeLayout{railPFs: map[string][]string{}, vfPF: map[string]string{}}
		specIfaces, _, _ := unstructured.NestedSlice(s.Object, "spec", "interfaces")
		for _, raw := range specIfaces {
			iface, _ := raw.(map[string]any)
			pf, _ := iface["pciAddress"].(string)
			groups, _ := iface["vfGroups"].([]any)
			for _, g := range groups {
				group, _ := g.(map[string]any)
				name, _ := group["resourceName"].(string)
				if rail, ok := rails[name]; ok && !slices.Contains(l.railPFs[rail], pf) {
					l.railPFs[rail] = append(l.railPFs[rail], pf)
				}
			}
		}
		statusIfaces, _, _ := unstructured.NestedSlice(s.Object, "status", "interfaces")
		for _, raw := range statusIfaces {
			iface, _ := raw.(map[string]any)
			pf, _ := iface["pciAddress"].(string)
			vfs, _ := iface["Vfs"].([]any) // the operator's status field is capitalized
			for _, v := range vfs {
				vf, _ := v.(map[string]any)
				if addr, _ := vf["pciAddress"].(string); addr != "" {
					l.vfPF[addr] = pf
				}
			}
		}
		layouts[s.GetName()] = l
	}
	return layouts
}

// railForVF returns the rail whose single PF owns the VF; multi-PF rails never match.
func (l sriovNodeLayout) railForVF(vf string) string {
	pf := l.vfPF[vf]
	for rail, pfs := range l.railPFs {
		if pf != "" && len(pfs) == 1 && pfs[0] == pf {
			return rail
		}
	}
	return ""
}

// multiPFRails reports rails that span more than one PF on any node: a VF from
// either PF may be allocated, so the rail cannot be tied to one GPU.
func multiPFRails(layouts map[string]sriovNodeLayout) map[string]string {
	multi := make(map[string]string)
	for _, node := range slices.Sorted(maps.Keys(layouts)) {
		for rail, pfs := range layouts[node].railPFs {
			if len(pfs) > 1 && multi[rail] == "" {
				multi[rail] = fmt.Sprintf("pool %s spans %d PFs on %s (%s)", rail, len(pfs), node, strings.Join(pfs, ", "))
			}
		}
	}
	return multi
}
