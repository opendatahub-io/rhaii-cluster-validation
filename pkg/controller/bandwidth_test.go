package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks/networking"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks/rdma"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/config"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/jobrunner"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes/fake"
)

func TestResolveStarNodes(t *testing.T) {
	tests := []struct {
		name           string
		serverNode     string
		clientNodes    []string
		gpuNodes       []string
		wantServer     string
		wantClientSize int
	}{
		{
			name:           "defaults to first node as server, rest as clients",
			gpuNodes:       []string{"node-a", "node-b", "node-c"},
			wantServer:     "node-a",
			wantClientSize: 2,
		},
		{
			name:           "explicit server node",
			serverNode:     "node-b",
			gpuNodes:       []string{"node-a", "node-b", "node-c"},
			wantServer:     "node-b",
			wantClientSize: 2,
		},
		{
			name:           "explicit server and client nodes are respected as-is",
			serverNode:     "node-b",
			clientNodes:    []string{"node-c"},
			gpuNodes:       []string{"node-a", "node-b", "node-c"},
			wantServer:     "node-b",
			wantClientSize: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := newTestController(nil)
			c.opts.ServerNode = tt.serverNode
			c.opts.ClientNodes = tt.clientNodes

			server, clients := c.resolveStarNodes(tt.gpuNodes)
			if server != tt.wantServer {
				t.Errorf("server = %q, want %q", server, tt.wantServer)
			}
			if len(clients) != tt.wantClientSize {
				t.Errorf("clients = %v, want %d entries", clients, tt.wantClientSize)
			}
			for _, cl := range clients {
				if cl == server {
					t.Errorf("client list should not include the server node %q", server)
				}
			}
		})
	}
}

func TestRunBandwidthJobs_NoJobsRegistered(t *testing.T) {
	c, buf := newTestController(nil)

	results, err := c.runBandwidthJobs(context.Background(), []string{"node-a", "node-b"}, nil)
	if err != nil {
		t.Fatalf("runBandwidthJobs() error = %v", err)
	}
	if results != nil {
		t.Errorf("expected no results, got %v", results)
	}
	if !strings.Contains(buf.String(), "No jobs registered") {
		t.Errorf("expected skip message, got output: %s", buf.String())
	}
}

func TestRunBandwidthJobs_AMDGPUSkipsJobs(t *testing.T) {
	c, buf := newTestController(nil)
	c.AddJob(networking.NewIperfJob(5, 1, nil))
	c.gpuVendor = config.GPUVendorAMD

	results, err := c.runBandwidthJobs(context.Background(), []string{"node-a", "node-b"}, nil)
	if err != nil {
		t.Fatalf("runBandwidthJobs() error = %v", err)
	}
	if results != nil {
		t.Errorf("expected no results for AMD GPUs, got %v", results)
	}
	if !strings.Contains(buf.String(), "AMD GPU detected") {
		t.Errorf("expected AMD skip message, got output: %s", buf.String())
	}
}

func TestRunBandwidthJobs_RequiresAtLeastTwoNodes(t *testing.T) {
	c, _ := newTestController(nil)
	c.AddJob(networking.NewIperfJob(5, 1, nil))

	_, err := c.runBandwidthJobs(context.Background(), []string{"only-node"}, nil)
	if err == nil {
		t.Fatal("expected error when fewer than 2 GPU nodes are available")
	}
	if !strings.Contains(err.Error(), "need at least 2 GPU nodes") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestExpandRDMAJobs_EFA(t *testing.T) {
	newController := func() *Controller {
		client := fake.NewSimpleClientset( //nolint:staticcheck
			gpuNodeWithEFA("node-a", nil, "nvidia.com/gpu", 2, 4),
			gpuNodeWithEFA("node-b", nil, "nvidia.com/gpu", 2, 4),
		)
		c, _ := newTestController(client)
		cfg, err := config.GetConfig(config.PlatformEKS)
		if err != nil {
			t.Fatalf("GetConfig(EKS) error = %v", err)
		}
		c.cfg = cfg
		c.gpuResource = corev1.ResourceName("nvidia.com/gpu")
		c.gpuCounts = map[string]int64{"node-a": 2, "node-b": 2}
		base := rdma.NewRDMABandwidthJob(0, 0, nil)
		base.PodCfg = &jobrunner.PodConfig{
			ResourceRequests: map[string]string{"cpu": "500m"},
			ResourceLimits:   map[string]string{},
			Privileged:       true,
		}
		c.AddJob(base)
		return c
	}

	topology := func(devices ...string) *checks.NodeTopology {
		topo := &checks.NodeTopology{GPUCount: 2, NICCount: len(devices)}
		for i, device := range devices {
			gpuID := i / 2
			nic := checks.NICInfo{Dev: device, LinkLayer: checks.LinkLayerSRD}
			topo.NICList = append(topo.NICList, nic)
			topo.Pairs = append(topo.Pairs, checks.GPUNICPair{
				GPU: checks.GPUInfo{ID: gpuID},
				NIC: nic,
			})
		}
		return topo
	}

	t.Run("expands ordered GPU groups and WEP", func(t *testing.T) {
		c := newController()
		jobs, skips := c.expandRDMAJobs(context.Background(), []string{"node-a", "node-b"}, map[string]*checks.NodeTopology{
			"node-a": topology("rdmap1", "rdmap0", "rdmap3", "rdmap2"),
			"node-b": topology("rdmap5", "rdmap4", "rdmap7", "rdmap6"),
		}, nil)
		if len(skips) != 0 {
			t.Fatalf("unexpected skips: %#v", skips)
		}
		wantNames := []string{"efa-rma-bw-gpu0", "efa-rma-bw-gpu1", "efa-rma-bw-wep"}
		if len(jobs) != len(wantNames) {
			t.Fatalf("got %d jobs, want %d", len(jobs), len(wantNames))
		}
		for i, want := range wantNames {
			if jobs[i].Name() != want {
				t.Errorf("jobs[%d].Name() = %q, want %q", i, jobs[i].Name(), want)
			}
		}

		pdJob := jobs[0].(*rdma.EFABandwidthJob)
		if got := pdJob.LanesByNode["node-a"]; len(got) != 2 || got[0].Device != "rdmap0" || got[1].Device != "rdmap1" {
			t.Errorf("GPU0 lanes = %#v, want rdmap0,rdmap1", got)
		}
		spec, err := pdJob.ServerSpec("node-a", "rhaii-validation", "tools:latest")
		if err != nil {
			t.Fatalf("ServerSpec() error = %v", err)
		}
		container := spec.Spec.Template.Spec.Containers[0]
		if got := container.Resources.Requests[config.EFAResourceName]; got.Value() != 4 {
			t.Errorf("EFA request = %s, want 4", got.String())
		}
		if got := container.Resources.Requests[corev1.ResourceName("nvidia.com/gpu")]; got.Value() != 2 {
			t.Errorf("GPU request = %s, want 2", got.String())
		}
		if container.SecurityContext == nil || container.SecurityContext.Privileged != nil {
			t.Errorf("EFA security context = %#v, want non-privileged", container.SecurityContext)
		}
	})

	t.Run("skips incompatible group and WEP", func(t *testing.T) {
		c := newController()
		jobs, skips := c.expandRDMAJobs(context.Background(), []string{"node-a", "node-b"}, map[string]*checks.NodeTopology{
			"node-a": topology("rdmap0", "rdmap1", "rdmap2", "rdmap3"),
			"node-b": topology("rdmap4", "rdmap5", "rdmap6"),
		}, nil)
		if len(jobs) != 1 || jobs[0].Name() != "efa-rma-bw-gpu0" {
			t.Errorf("jobs = %#v, want only compatible GPU0 job", jobs)
		}
		if len(skips) != 2 || skips[0].JobName != "efa-rma-bw-gpu1" || skips[1].JobName != "efa-rma-bw-wep" {
			t.Errorf("skips = %#v, want GPU1 and WEP skips", skips)
		}
	})

	t.Run("honors configured EFA count", func(t *testing.T) {
		c := newController()
		c.cfg.Jobs.Requests[string(config.EFAResourceName)] = "2"
		jobs, skips := c.expandRDMAJobs(context.Background(), []string{"node-a", "node-b"}, map[string]*checks.NodeTopology{
			"node-a": topology("rdmap0", "rdmap1"),
			"node-b": topology("rdmap4", "rdmap5"),
		}, nil)
		if len(skips) != 0 || len(jobs) == 0 {
			t.Fatalf("jobs = %#v, skips = %#v", jobs, skips)
		}
		spec, err := jobs[0].ServerSpec("node-a", "rhaii-validation", "tools:latest")
		if err != nil {
			t.Fatalf("ServerSpec() error = %v", err)
		}
		if got := spec.Spec.Template.Spec.Containers[0].Resources.Requests[config.EFAResourceName]; got.Value() != 2 {
			t.Errorf("EFA request = %s, want configured count 2", got.String())
		}
	})
}

// nodeState mirrors a SriovNetworkNodeState: each PF's single group owns VFs 0-7, and
// status lists the PF's VFs in vfID order.
func nodeState(node string, pfRes map[string]string, pfVFs map[string][]string) *unstructured.Unstructured {
	groups := make(map[string][]any, len(pfRes))
	for pf, res := range pfRes {
		groups[pf] = []any{map[string]any{"resourceName": res, "vfRange": "0-7"}}
	}
	return nodeStateGroups(node, groups, pfVFs)
}

func nodeStateGroups(node string, pfGroups map[string][]any, pfVFs map[string][]string) *unstructured.Unstructured {
	var specIfaces []any
	for pf, groups := range pfGroups {
		specIfaces = append(specIfaces, map[string]any{"pciAddress": pf, "vfGroups": groups})
	}
	var statusIfaces []any
	for pf, vfs := range pfVFs {
		var vfObjs []any
		for id, vf := range vfs {
			vfObjs = append(vfObjs, map[string]any{"pciAddress": vf, "vfID": int64(id)})
		}
		statusIfaces = append(statusIfaces, map[string]any{
			"pciAddress": pf,
			"Vfs":        vfObjs,
		})
	}
	u := &unstructured.Unstructured{Object: map[string]any{
		"spec":   map[string]any{"interfaces": specIfaces},
		"status": map[string]any{"interfaces": statusIfaces},
	}}
	u.SetAPIVersion("sriovnetwork.openshift.io/v1")
	u.SetKind("SriovNetworkNodeState")
	u.SetNamespace("openshift-sriov-network-operator")
	u.SetName(node)
	return u
}

// sriovBandwidthController seeds rail plans for p6/p8 (resolution itself is covered in
// sriovrdma_test.go) and gives both nodes a topology whose NICs are the given VFs.
func sriovBandwidthController(t *testing.T, states []*unstructured.Unstructured, nicPCIs ...string) (*Controller, map[string]*checks.NodeTopology) {
	t.Helper()
	c, _ := newTestController(nil)
	cfg, err := config.GetConfig(config.PlatformOCP)
	if err != nil {
		t.Fatalf("GetConfig(OCP) error = %v", err)
	}
	c.cfg = cfg
	c.gpuNodes = []string{"node-a", "node-b"}
	c.sriovResolved = true
	c.sriovRDMAPlans = []sriovRDMAPlan{
		{resource: "openshift.io/p6rdma", network: operatorNS + "/roce-p6"},
		{resource: "openshift.io/p8rdma", network: operatorNS + "/roce-p8"},
	}
	c.dynamic = newFakeDynamic(t, states...)
	base := rdma.NewRDMABandwidthJob(0, 0, nil)
	base.PodCfg = &jobrunner.PodConfig{
		ResourceRequests: map[string]string{"cpu": "500m"},
		ResourceLimits:   map[string]string{},
		Annotations:      map[string]string{},
	}
	c.AddJob(base)

	topo := func() *checks.NodeTopology {
		topo := &checks.NodeTopology{GPUCount: len(nicPCIs), NICCount: len(nicPCIs)}
		for i, pci := range nicPCIs {
			nic := checks.NICInfo{Dev: fmt.Sprintf("mlx5_%d", 18+i), PCIAddr: pci, LinkLayer: checks.LinkLayerEthernet}
			topo.NICList = append(topo.NICList, nic)
			// Stored reports keep only the device name in pairs.
			topo.Pairs = append(topo.Pairs, checks.GPUNICPair{GPU: checks.GPUInfo{ID: i}, NIC: checks.NICInfo{Dev: nic.Dev}})
		}
		return topo
	}
	return c, map[string]*checks.NodeTopology{"node-a": topo(), "node-b": topo()}
}

func TestExpandRDMAJobs_SRIOV(t *testing.T) {
	nodeA := "node-a"
	pfRes := map[string]string{"0000:19:00.0": "p6rdma", "0000:29:00.0": "p8rdma"}
	pfVFs := map[string][]string{"0000:19:00.0": {"0000:19:00.2"}, "0000:29:00.0": {"0000:29:00.2"}}
	c, topoMap := sriovBandwidthController(t,
		[]*unstructured.Unstructured{nodeState("node-a", pfRes, pfVFs), nodeState("node-b", pfRes, pfVFs)},
		"0000:19:00.2", "0000:29:00.2")

	jobs, skips := c.expandRDMAJobs(context.Background(), []string{"node-a", "node-b"}, topoMap, nil)
	if len(skips) != 0 {
		t.Fatalf("unexpected skips: %#v", skips)
	}
	if len(jobs) != 3 { // 2 PD jobs + 1 WEP job
		t.Fatalf("jobs count = %d, want 3 (2 PD + 1 WEP)", len(jobs))
	}

	pd0, ok := jobs[0].(*rdma.RDMABandwidthJob)
	if !ok || pd0.Rail != "openshift.io/p6rdma" {
		t.Fatalf("job[0] = %#v, want Rail=openshift.io/p6rdma", jobs[0])
	}
	if pd0.PodCfg.ResourceRequests["openshift.io/p6rdma"] != "1" {
		t.Errorf("PD0 resource request = %q, want 1", pd0.PodCfg.ResourceRequests["openshift.io/p6rdma"])
	}
	if pd0.PodCfg.Annotations[config.MultusNetworksAnnotation] != operatorNS+"/roce-p6" {
		t.Errorf("PD0 multus annotation = %q", pd0.PodCfg.Annotations[config.MultusNetworksAnnotation])
	}
	spec, err := pd0.ServerSpec(nodeA, "rhaii-validation", "tools:latest")
	if err != nil {
		t.Fatalf("ServerSpec error: %v", err)
	}
	srvCmd := strings.Join(spec.Spec.Template.Spec.Containers[0].Command, " ")
	if !strings.Contains(srvCmd, "resolve_dev PCIDEVICE_OPENSHIFT_IO_P6RDMA") {
		t.Errorf("server command must resolve device dynamically: %s", srvCmd)
	}
	if strings.Contains(srvCmd, "mlx5_18") {
		t.Errorf("rail mode must not pass rdma-node's device name (ib_write_bw rejects a second -d): %s", srvCmd)
	}

	wep, ok := jobs[2].(*rdma.RDMAWEPJob)
	if !ok {
		t.Fatalf("job[2] = %#v, want *RDMAWEPJob", jobs[2])
	}
	// WEP must request exactly 1 VF per rail, not 2 per rail.
	if got := wep.PodCfg.ResourceRequests["openshift.io/p6rdma"]; got != "1" {
		t.Errorf("WEP p6rdma request = %q, want 1", got)
	}
	if got := wep.PodCfg.ResourceRequests["openshift.io/p8rdma"]; got != "1" {
		t.Errorf("WEP p8rdma request = %q, want 1", got)
	}
	wepNets := wep.PodCfg.Annotations[config.MultusNetworksAnnotation]
	if !strings.Contains(wepNets, "roce-p6") || !strings.Contains(wepNets, "roce-p8") {
		t.Errorf("WEP networks = %q, want both roce-p6 and roce-p8", wepNets)
	}
}

func TestExpandRDMAJobs_SRIOVPoolsSplitOnePF(t *testing.T) {
	// One PF split by vfRange: VFs 0-3 belong to p6rdma, 4-7 to p8rdma.
	pf := "0000:19:00.0"
	vfs := []string{"0000:19:00.2", "0000:19:00.3", "0000:19:00.4", "0000:19:00.5", "0000:19:00.6", "0000:19:00.7", "0000:19:01.0", "0000:19:01.1"}
	groups := map[string][]any{pf: {
		map[string]any{"resourceName": "p6rdma", "vfRange": "0-3"},
		map[string]any{"resourceName": "p8rdma", "vfRange": "4-7"},
	}}
	states := []*unstructured.Unstructured{
		nodeStateGroups("node-a", groups, map[string][]string{pf: vfs}),
		nodeStateGroups("node-b", groups, map[string][]string{pf: vfs}),
	}
	// Map iteration order is random, so a wrong lookup shows up across repeated runs.
	for range 20 {
		c, topoMap := sriovBandwidthController(t, states, vfs[1], vfs[5]) // vfID 1 and vfID 5
		jobs, _ := c.expandRDMAJobs(context.Background(), []string{"node-a", "node-b"}, topoMap, nil)
		var rails []string
		for _, j := range jobs {
			if pd, ok := j.(*rdma.RDMABandwidthJob); ok {
				rails = append(rails, pd.Rail)
			}
		}
		if !slices.Equal(rails, []string{"openshift.io/p6rdma", "openshift.io/p8rdma"}) {
			t.Fatalf("PD rails = %v, want [p6rdma p8rdma] from each VF's vfRange", rails)
		}
	}
}

func TestExpandRDMAJobs_SRIOVUnmappedLanesAreSkippedNotRunOnStaleDevices(t *testing.T) {
	pfVFs := map[string][]string{"0000:19:00.0": {"0000:19:00.2"}, "0000:29:00.0": {"0000:29:00.2"}}
	tests := []struct {
		name   string
		states []*unstructured.Unstructured
		reason string
	}{
		{
			name: "pool spans two PFs",
			states: []*unstructured.Unstructured{
				nodeState("node-a", map[string]string{"0000:19:00.0": "p6rdma", "0000:29:00.0": "p6rdma"}, pfVFs),
				nodeState("node-b", map[string]string{"0000:19:00.0": "p6rdma", "0000:29:00.0": "p6rdma"}, pfVFs),
			},
			reason: "spans 2 PFs",
		},
		{
			name:   "no SriovNetworkNodeState",
			reason: "SriovNetworkNodeState",
		},
		{
			name: "VF not owned by a detected rail",
			states: []*unstructured.Unstructured{
				nodeState("node-a", map[string]string{"0000:19:00.0": "p6rdma", "0000:29:00.0": "storage"}, pfVFs),
				nodeState("node-b", map[string]string{"0000:19:00.0": "p6rdma", "0000:29:00.0": "storage"}, pfVFs),
			},
			reason: "0000:29:00.2",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, topoMap := sriovBandwidthController(t, tt.states, "0000:19:00.2", "0000:29:00.2")
			jobs, skips := c.expandRDMAJobs(context.Background(), []string{"node-a", "node-b"}, topoMap, nil)
			for _, j := range jobs {
				if pd, ok := j.(*rdma.RDMABandwidthJob); ok && pd.Rail == "" {
					t.Errorf("job %s would run on rdma-node's device %s without a VF", pd.Name(), pd.Device)
				}
				if _, ok := j.(*rdma.RDMAWEPJob); ok {
					t.Errorf("WEP must not run when a lane has no rail")
				}
			}
			var text []string
			for _, s := range skips {
				if s.Status != checks.StatusSkip {
					t.Errorf("skip %s has status %s", s.JobName, s.Status)
				}
				text = append(text, s.JobName+": "+s.Message)
			}
			if !strings.Contains(strings.Join(text, "\n"), tt.reason) {
				t.Errorf("skips must explain %q, got:\n%s", tt.reason, strings.Join(text, "\n"))
			}
		})
	}
}
