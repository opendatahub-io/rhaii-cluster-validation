package networking

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
)

// TCPBandwidthCheck validates TCP bandwidth between nodes using iperf3.
type TCPBandwidthCheck struct {
	nodeName  string
	serverIP  string
	threshold float64 // Gbps
}

func NewTCPBandwidthCheck(nodeName, serverIP string, threshold float64) *TCPBandwidthCheck {
	return &TCPBandwidthCheck{
		nodeName:  nodeName,
		serverIP:  serverIP,
		threshold: threshold,
	}
}

func (c *TCPBandwidthCheck) Name() string     { return "tcp_bandwidth_cross_node" }
func (c *TCPBandwidthCheck) Category() string { return "networking" }

func (c *TCPBandwidthCheck) Run(ctx context.Context) checks.Result {
	r := checks.Result{
		Node:     c.nodeName,
		Category: c.Category(),
		Name:     c.Name(),
	}

	if c.serverIP == "" {
		r.Status = checks.StatusSkip
		r.Message = "No iperf3 server IP provided, skipping bandwidth test"
		return r
	}

	output, err := exec.CommandContext(ctx, "iperf3",
		"-c", c.serverIP,
		"-t", "10",
		"-J").Output()
	if err != nil {
		r.Status = checks.StatusFail
		r.Message = fmt.Sprintf("iperf3 test failed: %v", err)
		r.Remediation = "Check network connectivity between nodes"
		return r
	}

	bw, err := parseIperfOutput(output)
	if err != nil {
		r.Status = checks.StatusFail
		r.Message = "Failed to parse iperf3 JSON output"
		return r
	}

	r.Details = map[string]any{
		"bandwidth_gbps": fmt.Sprintf("%.1f", bw.Gbps),
		"retransmits":    bw.Retransmits,
		"server_ip":      c.serverIP,
	}

	switch {
	case bw.Gbps >= c.threshold:
		r.Status = checks.StatusPass
		r.Message = fmt.Sprintf("TCP bandwidth: %.1f Gbps (threshold: %.0f Gbps)", bw.Gbps, c.threshold)
	case bw.Gbps >= c.threshold*0.4:
		r.Status = checks.StatusWarn
		r.Message = fmt.Sprintf("TCP bandwidth: %.1f Gbps (below %.0f Gbps threshold)", bw.Gbps, c.threshold)
	default:
		r.Status = checks.StatusFail
		r.Message = fmt.Sprintf("TCP bandwidth: %.1f Gbps (well below %.0f Gbps threshold)", bw.Gbps, c.threshold)
		r.Remediation = "Check VM SKU network limits and node placement"
	}

	return r
}

// bandwidthResult holds parsed iperf3 bandwidth data.
type bandwidthResult struct {
	Gbps        float64
	Retransmits int
}

// parseIperfOutput parses iperf3 JSON output and returns bandwidth in Gbps.
func parseIperfOutput(data []byte) (*bandwidthResult, error) {
	var result iperfResult
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return &bandwidthResult{
		Gbps:        result.End.SumSent.BitsPerSecond / 1e9,
		Retransmits: result.End.SumSent.Retransmits,
	}, nil
}

type iperfResult struct {
	End struct {
		SumSent struct {
			BitsPerSecond float64 `json:"bits_per_second"`
			Retransmits   int     `json:"retransmits"`
		} `json:"sum_sent"`
	} `json:"end"`
}
