package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks/networking"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/config"
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
