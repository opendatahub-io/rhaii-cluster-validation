package rdma

import (
	"fmt"
	"strings"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/jobrunner"

	batchv1 "k8s.io/api/batch/v1"
)

// RDMAWEPJob implements the Job interface for whole-endpoint RDMA bandwidth testing.
// It runs ib_write_bw on ALL NICs in parallel from a single pod and sums the results.
type RDMAWEPJob struct {
	Duration      int
	PassThreshold float64
	WarnThreshold float64
	PodCfg        *jobrunner.PodConfig
	ServerImage   string
	ClientImage   string
	Devices       []string // all NIC devices (e.g., ["mlx5_0", "mlx5_1", ..., "mlx5_7"])
	Rails         []string // SR-IOV resources, 1:1 with Devices; resolves each NIC at runtime
	GPUIDs        []int    // matching GPU IDs for --use_cuda
	QPs           int      // number of queue pairs per NIC (default 4)
	MessageSize   int      // message size in bytes (default 1048576 = 1 MiB)
}

func NewRDMAWEPJob(pass, warn float64, devices []string, gpuIDs []int) *RDMAWEPJob {
	return &RDMAWEPJob{
		Duration:      10,
		PassThreshold: pass,
		WarnThreshold: warn,
		Devices:       devices,
		GPUIDs:        gpuIDs,
		QPs:           DefaultRDMAQPs,
		MessageSize:   DefaultRDMAMessageSize,
	}
}

func (j *RDMAWEPJob) Name() string { return "ib-write-bw-wep" }

func (j *RDMAWEPJob) SetPodConfig(cfg *jobrunner.PodConfig) {
	if cfg == nil {
		cfg = &jobrunner.PodConfig{
			ResourceRequests: make(map[string]string),
			ResourceLimits:   make(map[string]string),
		}
	}
	if cfg.ResourceLimits == nil {
		cfg.ResourceLimits = make(map[string]string)
	}

	// WEP needs ALL devices, not just 1 — scale device resources to NIC count
	nicCount := fmt.Sprintf("%d", len(j.Devices))
	for k := range cfg.ResourceRequests {
		if k == "cpu" || k == "memory" {
			continue
		}
		// Scale GPU and RDMA resources to match number of NICs
		cfg.ResourceRequests[k] = nicCount
		cfg.ResourceLimits[k] = nicCount
	}

	// Device resources must have equal requests and limits
	for k, v := range cfg.ResourceRequests {
		if k == "cpu" || k == "memory" {
			continue
		}
		if _, ok := cfg.ResourceLimits[k]; !ok {
			cfg.ResourceLimits[k] = v
		}
	}

	cfg.Privileged = true
	j.PodCfg = cfg
}

func (j *RDMAWEPJob) SetThreshold(pass, warn float64) {
	j.PassThreshold = pass
	j.WarnThreshold = warn
}

func (j *RDMAWEPJob) GetServerImage() string    { return j.ServerImage }
func (j *RDMAWEPJob) GetClientImage() string    { return j.ClientImage }
func (j *RDMAWEPJob) SetServerImage(img string) { j.ServerImage = img }
func (j *RDMAWEPJob) SetClientImage(img string) { j.ClientImage = img }

// buildScript generates a bash script that runs ib_write_bw on all NICs in parallel.
// Server: starts ib_write_bw on each NIC with different ports.
// Client: connects to each server port in parallel, waits for all, sums bandwidth.
func (j *RDMAWEPJob) ibArgs() string {
	args := fmt.Sprintf("--duration %d", j.Duration)
	if j.QPs > 0 {
		args += fmt.Sprintf(" --qp %d", j.QPs)
	}
	if j.MessageSize > 0 {
		args += fmt.Sprintf(" --size %d", j.MessageSize)
	}
	return args
}

func (j *RDMAWEPJob) serverScript() []string {
	base := j.ibArgs()
	var sb strings.Builder
	sb.WriteString("#!/bin/bash\n")
	if len(j.Rails) > 0 {
		sb.WriteString(resolveDevFunc() + "\n")
	}
	for i, dev := range j.Devices {
		port := 18515 + i
		devRef := dev
		if i < len(j.Rails) && j.Rails[i] != "" {
			devVar := fmt.Sprintf("d%d", i)
			fmt.Fprintf(&sb, "%s=$(resolve_dev %s)\n", devVar, railEnvKey(j.Rails[i]))
			devRef = "$" + devVar
		} else if !checks.ValidDeviceName.MatchString(dev) {
			continue
		}
		cuda := ""
		if i < len(j.GPUIDs) && j.GPUIDs[i] >= 0 {
			cuda = fmt.Sprintf(" --use_cuda %d", j.GPUIDs[i])
		}
		fmt.Fprintf(&sb, "[ -n \"%s\" ] && ib_write_bw %s -d %s -p %d%s &\n", devRef, base, devRef, port, cuda)
	}
	sb.WriteString("wait\n")
	return []string{"bash", "-c", sb.String()}
}

func (j *RDMAWEPJob) clientScript(serverIP string) []string {
	base := j.ibArgs()
	var sb strings.Builder
	sb.WriteString("#!/bin/bash\nmkdir -p /tmp/wep\n")
	if len(j.Rails) > 0 {
		sb.WriteString(resolveDevFunc() + "\n")
	}
	for i, dev := range j.Devices {
		port := 18515 + i
		devRef := dev
		if i < len(j.Rails) && j.Rails[i] != "" {
			devVar := fmt.Sprintf("d%d", i)
			fmt.Fprintf(&sb, "%s=$(resolve_dev %s)\n", devVar, railEnvKey(j.Rails[i]))
			devRef = "$" + devVar
		} else if !checks.ValidDeviceName.MatchString(dev) {
			continue
		}
		cuda := ""
		if i < len(j.GPUIDs) && j.GPUIDs[i] >= 0 {
			cuda = fmt.Sprintf(" --use_cuda %d", j.GPUIDs[i])
		}
		fmt.Fprintf(&sb, "if [ -n \"%s\" ]; then\n  ib_write_bw %s -d %s -p %d%s %s > /tmp/wep/nic%d.txt 2>&1 &\nelse\n  echo 'no device' > /tmp/wep/nic%d.txt\nfi\n",
			devRef, base, devRef, port, cuda, serverIP, i, i)
	}
	sb.WriteString("wait\necho '=== WEP RESULTS ==='\n")
	for i, dev := range j.Devices {
		label := dev
		if i < len(j.Rails) && j.Rails[i] != "" {
			label = j.Rails[i]
		} else if !checks.ValidDeviceName.MatchString(dev) {
			continue
		}
		fmt.Fprintf(&sb, "echo '--- NIC %d: %s ---'\ncat /tmp/wep/nic%d.txt\n", i, label, i)
	}
	return []string{"bash", "-c", sb.String()}
}

func (j *RDMAWEPJob) ServerSpec(node, namespace, image string) (*batchv1.Job, error) {
	return jobrunner.BuildJobSpec(j.Name(), node, namespace, image, jobrunner.RoleServer, j.PodCfg,
		j.serverScript())
}

func (j *RDMAWEPJob) ClientSpec(node, namespace, image, serverIP string) (*batchv1.Job, error) {
	return jobrunner.BuildJobSpec(j.Name(), node, namespace, image, jobrunner.RoleClient, j.PodCfg,
		j.clientScript(serverIP))
}

// ParseResult parses output from all NICs and sums the bandwidth.
func (j *RDMAWEPJob) ParseResult(logs string) (*jobrunner.JobResult, error) {
	// Sum bandwidth across all NIC outputs
	var totalGbps float64
	var nicCount int

	sections := strings.Split(logs, "--- NIC ")
	for _, section := range sections[1:] { // skip header
		gbps, err := parseIBWriteBW(section)
		if err != nil {
			continue
		}
		totalGbps += gbps
		nicCount++
	}

	if nicCount == 0 {
		return nil, fmt.Errorf("no bandwidth values found in WEP output")
	}

	r := &jobrunner.JobResult{
		Details: map[string]any{
			"aggregate_bandwidth_gbps": fmt.Sprintf("%.1f", totalGbps),
			"nic_count":                nicCount,
			"per_nic_avg_gbps":         fmt.Sprintf("%.1f", totalGbps/float64(nicCount)),
		},
	}

	switch {
	case totalGbps >= j.PassThreshold:
		r.Status = checks.StatusPass
		r.Message = fmt.Sprintf("WEP RDMA bandwidth: %.1f Gbps across %d NICs (>= %.0f Gbps pass threshold)", totalGbps, nicCount, j.PassThreshold)
	case totalGbps >= j.WarnThreshold:
		r.Status = checks.StatusWarn
		r.Message = fmt.Sprintf("WEP RDMA bandwidth: %.1f Gbps across %d NICs (>= %.0f Gbps warn, < %.0f Gbps pass)", totalGbps, nicCount, j.WarnThreshold, j.PassThreshold)
	default:
		r.Status = checks.StatusFail
		r.Message = fmt.Sprintf("WEP RDMA bandwidth: %.1f Gbps across %d NICs (< %.0f Gbps warn threshold)", totalGbps, nicCount, j.WarnThreshold)
	}

	return r, nil
}
