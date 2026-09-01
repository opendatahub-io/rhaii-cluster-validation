package rdma

import (
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/jobrunner"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

const (
	DefaultEFARMAMessageSize = 4 * 1024 * 1024
	efaRMAPortBase           = 18515
)

var (
	efaLaneHeaderRE = regexp.MustCompile(`^(\d+) DEVICE ([A-Za-z0-9_.-]+) GPU (\d+) RC (\d+) ---$`)
	efaMBPerSecRE   = regexp.MustCompile(`MB/sec:\s*([0-9]+(?:\.[0-9]+)?)`)
)

// EFABandwidthLane binds one EFA device to its PCIe-aligned GPU.
type EFABandwidthLane struct {
	GPUID  int    `json:"gpu_id"`
	Device string `json:"device"`
}

// EFABandwidthLaneResult is the parsed result for one fi_rma_bw process.
type EFABandwidthLaneResult struct {
	Slot          int     `json:"slot"`
	GPUID         int     `json:"gpu_id"`
	Device        string  `json:"device"`
	ExitCode      int     `json:"exit_code"`
	BandwidthGbps float64 `json:"bandwidth_gbps,omitempty"`
}

// EFABandwidthJob runs one fi_rma_bw process per lane. The same type is used
// for an isolated per-GPU group and for whole-endpoint (WEP) execution.
type EFABandwidthJob struct {
	GPUID         int
	WEP           bool
	MessageSize   int
	PassThreshold float64
	WarnThreshold float64
	LanesByNode   map[string][]EFABandwidthLane
	PodCfgByNode  map[string]*jobrunner.PodConfig
	ServerImage   string
	ClientImage   string
}

// NewEFABandwidthJob creates an EFA bandwidth job and applies the 4 MiB
// fi_rma_bw default used when message_size is zero in platform config
func NewEFABandwidthJob(gpuID int, wep bool, pass, warn float64, lanesByNode map[string][]EFABandwidthLane, podCfgByNode map[string]*jobrunner.PodConfig) *EFABandwidthJob {
	return &EFABandwidthJob{
		GPUID:         gpuID,
		WEP:           wep,
		MessageSize:   DefaultEFARMAMessageSize,
		PassThreshold: pass,
		WarnThreshold: warn,
		LanesByNode:   lanesByNode,
		PodCfgByNode:  podCfgByNode,
	}
}

func (j *EFABandwidthJob) Name() string {
	if j.WEP {
		return "efa-rma-bw-wep"
	}
	return fmt.Sprintf("efa-rma-bw-gpu%d", j.GPUID)
}

// Use the existing ImageConfigurable path so jobrunner supplies the same tools
// image used by the other bandwidth jobs
func (j *EFABandwidthJob) GetServerImage() string    { return j.ServerImage }
func (j *EFABandwidthJob) GetClientImage() string    { return j.ClientImage }
func (j *EFABandwidthJob) SetServerImage(img string) { j.ServerImage = img }
func (j *EFABandwidthJob) SetClientImage(img string) { j.ClientImage = img }

// lanesForNode returns the PCIe-aligned EFA NIC/GPU pairs assigned to one node
// A PD job contains the NIC group for one GPU while WEP contains every group
func (j *EFABandwidthJob) lanesForNode(node string) ([]EFABandwidthLane, error) {
	lanes, ok := j.LanesByNode[node]
	if !ok || len(lanes) == 0 {
		return nil, fmt.Errorf("no EFA bandwidth lanes configured for node %s", node)
	}

	validated := append([]EFABandwidthLane(nil), lanes...)
	for _, lane := range validated {
		if lane.GPUID < 0 {
			return nil, fmt.Errorf("invalid GPU ID %d for EFA device %s", lane.GPUID, lane.Device)
		}
		if !checks.ValidDeviceName.MatchString(lane.Device) {
			return nil, fmt.Errorf("invalid EFA device name %q", lane.Device)
		}
	}
	return validated, nil
}

// commandForNode starts one fi_rma_bw process per lane so every NIC in the GPU
// group is exercised concurrently with its aligned GPU and a unique port
func (j *EFABandwidthJob) commandForNode(node, serverIP string) ([]string, error) {
	lanes, err := j.lanesForNode(node)
	if err != nil {
		return nil, err
	}
	if serverIP != "" && net.ParseIP(serverIP) == nil {
		return nil, fmt.Errorf("invalid server IP %q", serverIP)
	}
	if j.MessageSize <= 0 {
		return nil, fmt.Errorf("invalid EFA message size %d", j.MessageSize)
	}

	var script strings.Builder
	script.WriteString("#!/bin/bash\nset -u\nmkdir -p /tmp/efa-rma-bw\npids=()\n")
	for slot, lane := range lanes {
		fmt.Fprintf(&script, "(\n  FI_PROVIDER=efa FI_EFA_USE_DEVICE_RDMA=1 FI_EFA_IFACE=%q \\\n    fi_rma_bw -m -p efa -o writedata -E=%d -D cuda -i %d -S %d",
			lane.Device, efaRMAPortBase+slot, lane.GPUID, j.MessageSize)
		if serverIP != "" {
			fmt.Fprintf(&script, " %s", serverIP)
		}
		fmt.Fprintf(&script, " > /tmp/efa-rma-bw/lane_%d.log 2>&1\n  rc=$?\n  echo \"$rc\" > /tmp/efa-rma-bw/lane_%d.rc\n  exit \"$rc\"\n) &\npids[%d]=$!\n", slot, slot, slot)
	}

	script.WriteString("status=0\nfor pid in \"${pids[@]}\"; do\n  wait \"$pid\" || status=1\ndone\n")
	if serverIP == "" {
		script.WriteString("exit \"$status\"\n")
		return []string{"bash", "-c", script.String()}, nil
	}

	for slot, lane := range lanes {
		fmt.Fprintf(&script, "rc=$(cat /tmp/efa-rma-bw/lane_%d.rc 2>/dev/null || echo 255)\n", slot)
		fmt.Fprintf(&script, "printf '%%s\\n' \"--- EFA LANE %d DEVICE %s GPU %d RC $rc ---\"\n", slot, lane.Device, lane.GPUID)
		fmt.Fprintf(&script, "cat /tmp/efa-rma-bw/lane_%d.log 2>/dev/null || true\n", slot)
	}
	// Always emit all lane logs. ParseResult owns the aggregate PASS/FAIL decision.
	script.WriteString("exit 0\n")
	return []string{"bash", "-c", script.String()}, nil
}

func (j *EFABandwidthJob) buildSpec(node, namespace, image, serverIP string, role jobrunner.Role) (*batchv1.Job, error) {
	command, err := j.commandForNode(node, serverIP)
	if err != nil {
		return nil, err
	}
	podCfg, ok := j.PodCfgByNode[node]
	if !ok || podCfg == nil {
		return nil, fmt.Errorf("no EFA pod configuration for node %s", node)
	}

	job, err := jobrunner.BuildJobSpec(j.Name(), node, namespace, image, role, podCfg, command)
	if err != nil {
		return nil, err
	}
	container := &job.Spec.Template.Spec.Containers[0]
	if container.SecurityContext == nil {
		container.SecurityContext = &corev1.SecurityContext{}
	}
	if container.SecurityContext.Capabilities == nil {
		container.SecurityContext.Capabilities = &corev1.Capabilities{}
	}
	container.SecurityContext.Capabilities.Add = append(container.SecurityContext.Capabilities.Add, corev1.Capability("IPC_LOCK"))
	return job, nil
}

func (j *EFABandwidthJob) ServerSpec(node, namespace, image string) (*batchv1.Job, error) {
	return j.buildSpec(node, namespace, image, "", jobrunner.RoleServer)
}

func (j *EFABandwidthJob) ClientSpec(node, namespace, image, serverIP string) (*batchv1.Job, error) {
	return j.buildSpec(node, namespace, image, serverIP, jobrunner.RoleClient)
}

func (j *EFABandwidthJob) expectedLaneCount() int {
	for _, lanes := range j.LanesByNode {
		return len(lanes)
	}
	return 0
}

// parseEFARMAOutput validates and parses the per-lane sections emitted by the
// client wrapper after all fi_rma_bw processes have completed
func parseEFARMAOutput(logs string, expected int) ([]EFABandwidthLaneResult, error) {
	sections := strings.Split(logs, "--- EFA LANE ")
	results := make([]EFABandwidthLaneResult, 0, len(sections)-1)
	seenSlots := make(map[int]bool)
	for _, section := range sections[1:] {
		lineEnd := strings.IndexByte(section, '\n')
		if lineEnd < 0 {
			return nil, fmt.Errorf("malformed EFA lane header")
		}
		header := strings.TrimSpace(section[:lineEnd])
		match := efaLaneHeaderRE.FindStringSubmatch(header)
		if match == nil {
			return nil, fmt.Errorf("malformed EFA lane header %q", header)
		}

		slot, _ := strconv.Atoi(match[1])
		gpuID, _ := strconv.Atoi(match[3])
		exitCode, _ := strconv.Atoi(match[4])
		if seenSlots[slot] {
			return nil, fmt.Errorf("duplicate EFA lane slot %d", slot)
		}
		seenSlots[slot] = true

		result := EFABandwidthLaneResult{Slot: slot, Device: match[2], GPUID: gpuID, ExitCode: exitCode}
		measurements := efaMBPerSecRE.FindAllStringSubmatch(section[lineEnd+1:], -1)
		if exitCode == 0 {
			if len(measurements) != 1 {
				return nil, fmt.Errorf("EFA lane %d produced %d bandwidth measurements, want 1", slot, len(measurements))
			}
			mbPerSec, err := strconv.ParseFloat(measurements[0][1], 64)
			if err != nil {
				return nil, fmt.Errorf("invalid EFA bandwidth for lane %d: %w", slot, err)
			}
			result.BandwidthGbps = mbPerSec * 8 / 1000
		}
		results = append(results, result)
	}

	if len(results) != expected {
		return nil, fmt.Errorf("found %d EFA lane results, want %d", len(results), expected)
	}
	sort.Slice(results, func(i, k int) bool { return results[i].Slot < results[k].Slot })
	for slot, result := range results {
		if result.Slot != slot {
			return nil, fmt.Errorf("missing EFA lane slot %d", slot)
		}
	}
	return results, nil
}

func (j *EFABandwidthJob) ParseResult(logs string) (*jobrunner.JobResult, error) {
	laneResults, err := parseEFARMAOutput(logs, j.expectedLaneCount())
	if err != nil {
		return nil, err
	}

	var totalGbps float64
	failed := 0
	for _, lane := range laneResults {
		totalGbps += lane.BandwidthGbps
		if lane.ExitCode != 0 {
			failed++
		}
	}

	kind := "PD"
	if j.WEP {
		kind = "WEP"
	}
	result := &jobrunner.JobResult{Details: map[string]any{
		"fabric":                   "srd",
		"message_size":             j.MessageSize,
		"nic_count":                len(laneResults),
		"aggregate_bandwidth_gbps": fmt.Sprintf("%.1f", totalGbps),
		"lanes":                    laneResults,
	}}

	if failed > 0 {
		result.Status = checks.StatusFail
		result.Message = fmt.Sprintf("EFA %s bandwidth: %d/%d lane(s) failed", kind, failed, len(laneResults))
		return result, nil
	}

	switch {
	case totalGbps >= j.PassThreshold:
		result.Status = checks.StatusPass
		result.Message = fmt.Sprintf("EFA %s bandwidth: %.1f Gbps across %d NICs (>= %.0f Gbps pass threshold)", kind, totalGbps, len(laneResults), j.PassThreshold)
	case totalGbps >= j.WarnThreshold:
		result.Status = checks.StatusWarn
		result.Message = fmt.Sprintf("EFA %s bandwidth: %.1f Gbps across %d NICs (>= %.0f Gbps warn, < %.0f Gbps pass)", kind, totalGbps, len(laneResults), j.WarnThreshold, j.PassThreshold)
	default:
		result.Status = checks.StatusFail
		result.Message = fmt.Sprintf("EFA %s bandwidth: %.1f Gbps across %d NICs (< %.0f Gbps warn threshold)", kind, totalGbps, len(laneResults), j.WarnThreshold)
	}
	return result, nil
}
