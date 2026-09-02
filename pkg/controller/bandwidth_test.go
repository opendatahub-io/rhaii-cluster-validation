package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks/networking"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks/rdma"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/config"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/jobrunner"

	corev1 "k8s.io/api/core/v1"
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
