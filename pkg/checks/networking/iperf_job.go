package networking

import (
	"encoding/json"
	"fmt"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/jobrunner"

	batchv1 "k8s.io/api/batch/v1"
)

// IperfJob implements the Job interface for TCP bandwidth testing via iperf3.
type IperfJob struct {
	Duration  int                  // test duration in seconds (default 10)
	Threshold float64              // Gbps pass threshold
	PodCfg    *jobrunner.PodConfig // optional pod configuration
}

// NewIperfJob creates an iperf3 TCP bandwidth job.
func NewIperfJob(threshold float64, podCfg *jobrunner.PodConfig) *IperfJob {
	return &IperfJob{
		Duration:  10,
		Threshold: threshold,
		PodCfg:    podCfg,
	}
}

func (j *IperfJob) Name() string { return "iperf3-tcp" }

func (j *IperfJob) SetPodConfig(cfg *jobrunner.PodConfig) { j.PodCfg = cfg }
func (j *IperfJob) SetThreshold(t float64)               { j.Threshold = t }

func (j *IperfJob) ServerSpec(node, namespace, image string) (*batchv1.Job, error) {
	return jobrunner.BuildJobSpec(j.Name(), node, namespace, image, jobrunner.RoleServer, j.PodCfg,
		[]string{"iperf3", "-s"})
}

func (j *IperfJob) ClientSpec(node, namespace, image, serverIP string) (*batchv1.Job, error) {
	return jobrunner.BuildJobSpec(j.Name(), node, namespace, image, jobrunner.RoleClient, j.PodCfg,
		[]string{"iperf3", "-c", serverIP, "-t", fmt.Sprintf("%d", j.Duration), "-J"})
}

func (j *IperfJob) ParseResult(logs string) (*jobrunner.JobResult, error) {
	var result iperfJobOutput
	if err := json.Unmarshal([]byte(logs), &result); err != nil {
		return nil, fmt.Errorf("failed to parse iperf3 JSON: %w", err)
	}

	gbps := result.End.SumSent.BitsPerSecond / 1e9
	retransmits := result.End.SumSent.Retransmits

	r := &jobrunner.JobResult{
		Details: map[string]any{
			"bandwidth_gbps": fmt.Sprintf("%.1f", gbps),
			"retransmits":    retransmits,
		},
	}

	switch {
	case gbps >= j.Threshold:
		r.Status = checks.StatusPass
		r.Message = fmt.Sprintf("TCP bandwidth: %.1f Gbps (threshold: %.0f Gbps)", gbps, j.Threshold)
	case gbps >= j.Threshold*0.4:
		r.Status = checks.StatusWarn
		r.Message = fmt.Sprintf("TCP bandwidth: %.1f Gbps (below %.0f Gbps threshold)", gbps, j.Threshold)
	default:
		r.Status = checks.StatusFail
		r.Message = fmt.Sprintf("TCP bandwidth: %.1f Gbps (well below %.0f Gbps threshold)", gbps, j.Threshold)
	}

	return r, nil
}

type iperfJobOutput struct {
	End struct {
		SumSent struct {
			BitsPerSecond float64 `json:"bits_per_second"`
			Retransmits   int     `json:"retransmits"`
		} `json:"sum_sent"`
	} `json:"end"`
}
