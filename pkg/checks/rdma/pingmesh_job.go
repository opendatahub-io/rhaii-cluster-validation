package rdma

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/config"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/jobrunner"

	batchv1 "k8s.io/api/batch/v1"
)

const (
	pingmeshPortBase      = 18515
	defaultPingTimeout    = 10
	defaultPingIterations = 3
	defaultServerBufSec   = 30
	srdPingMessageSize    = 64 // fi_rdm_pingpong -S: fixed connectivity probe size
)

// PingMeshJob implements jobrunner.Job for pairwise RDMA connectivity testing.
// IB/RoCE uses ibv_rc_pingpong; EFA (SRD) uses fi_rdm_pingpong.
type PingMeshJob struct {
	ServerDevices []string // RDMA devices on the server (destination) node
	ClientDevices []string // RDMA devices on the client (source) node
	ServerNode    string   // server (destination) node name
	ClientNode    string   // client (source) node name
	RDMAType      config.RDMAType
	GIDIndex      int // -1 = auto-discover; >= 0 = fixed
	Iterations    int
	Timeout       int // per-test timeout in seconds
	PodCfg        *jobrunner.PodConfig
	ServerImage   string
	ClientImage   string
	// Rails are SR-IOV resources both pods request, e.g. openshift.io/p6rdma. When set,
	// each pod finds its own VF per rail at runtime and ServerDevices/ClientDevices are unused.
	Rails []string
}

func NewPingMeshJob(serverNode, clientNode string, serverDevs, clientDevs []string, rdmaType config.RDMAType, gidIndex, iterations, timeout int) *PingMeshJob {
	if iterations <= 0 {
		iterations = defaultPingIterations
	}
	if timeout <= 0 {
		timeout = defaultPingTimeout
	}
	return &PingMeshJob{
		ServerDevices: filterValidDeviceNames(serverDevs),
		ClientDevices: filterValidDeviceNames(clientDevs),
		ServerNode:    serverNode,
		ClientNode:    clientNode,
		RDMAType:      rdmaType,
		GIDIndex:      gidIndex,
		Iterations:    iterations,
		Timeout:       timeout,
	}
}

// ValidateDevices returns an error when no valid RDMA devices remain after
// NewPingMeshJob filters invalid names.
func (j *PingMeshJob) ValidateDevices() error {
	if len(j.Rails) > 0 {
		return nil
	}
	if len(j.ServerDevices) == 0 {
		return fmt.Errorf("pingmesh: no valid server RDMA devices")
	}
	if len(j.ClientDevices) == 0 {
		return fmt.Errorf("pingmesh: no valid client RDMA devices")
	}
	return nil
}

func (j *PingMeshJob) Name() string { return "pingmesh" }

func (j *PingMeshJob) SetPodConfig(cfg *jobrunner.PodConfig) {
	if cfg == nil {
		cfg = &jobrunner.PodConfig{}
	}
	clone := cfg.Clone()
	clone.Privileged = true
	// Device resources (GPU, RDMA) must have equal requests and limits (K8s requirement)
	if clone.ResourceLimits == nil {
		clone.ResourceLimits = make(map[string]string)
	}
	for k, v := range clone.ResourceRequests {
		if k == "cpu" || k == "memory" {
			continue
		}
		if _, ok := clone.ResourceLimits[k]; !ok {
			clone.ResourceLimits[k] = v
		}
	}
	j.PodCfg = clone
}

func (j *PingMeshJob) SetNameSuffix(suffix string) {
	if j.PodCfg == nil {
		j.PodCfg = &jobrunner.PodConfig{}
	}
	j.PodCfg.NameSuffix = suffix
}

// SetExtendedResource sets an extended resource request/limit on the pod config.
// Currently used for EFA resource injection where both request and limit must be equal.
func (j *PingMeshJob) SetExtendedResource(resourceName string, quantity string) {
	if j.PodCfg == nil {
		j.PodCfg = &jobrunner.PodConfig{}
	}
	if j.PodCfg.ResourceRequests == nil {
		j.PodCfg.ResourceRequests = make(map[string]string)
	}
	if j.PodCfg.ResourceLimits == nil {
		j.PodCfg.ResourceLimits = make(map[string]string)
	}
	j.PodCfg.ResourceRequests[resourceName] = quantity
	j.PodCfg.ResourceLimits[resourceName] = quantity
}

func (j *PingMeshJob) GetServerImage() string    { return j.ServerImage }
func (j *PingMeshJob) GetClientImage() string    { return j.ClientImage }
func (j *PingMeshJob) SetServerImage(img string) { j.ServerImage = img }
func (j *PingMeshJob) SetClientImage(img string) { j.ClientImage = img }

func (j *PingMeshJob) validDeviceCount(devs []string) int {
	n := 0
	for _, d := range devs {
		if checks.ValidDeviceName.MatchString(d) {
			n++
		}
	}
	return n
}

func filterValidDeviceNames(devs []string) []string {
	var out []string
	for _, d := range devs {
		if checks.ValidDeviceName.MatchString(d) {
			out = append(out, d)
		}
	}
	return out
}

func bashQuotedArray(name string, devs []string) string {
	if len(devs) == 0 {
		return name + "=()"
	}
	quoted := make([]string, len(devs))
	for i, d := range devs {
		quoted[i] = fmt.Sprintf("%q", d)
	}
	return fmt.Sprintf("%s=(%s)", name, strings.Join(quoted, " "))
}

func (j *PingMeshJob) serverTimeout() int {
	tests := len(j.endpoints(j.ServerDevices)) * len(j.endpoints(j.ClientDevices))
	return tests*j.Timeout + defaultServerBufSec
}

// railEnvKey is the env var the SR-IOV device plugin sets for a resource:
// openshift.io/p6rdma -> PCIDEVICE_OPENSHIFT_IO_P6RDMA (plus an _INFO JSON twin).
func railEnvKey(resource string) string {
	return "PCIDEVICE_" + strings.ToUpper(strings.NewReplacer(".", "_", "/", "_").Replace(resource))
}

// resolveDevFunc prints the pod's own RDMA device for one rail, or nothing when
// it cannot pick exactly one (a VF name differs per pod, so it is never baked in).
func resolveDevFunc() string {
	return `resolve_dev() {
  local d="" info pci
  info=$(printenv "$1_INFO")
  [ -n "$info" ] && d=$(printf %s "$info" | jq -r '[.[] | .rdma.rdma_dev // empty] | if length == 1 then .[0] else "" end' 2>/dev/null)
  if [ -z "$d" ]; then
    pci=$(printenv "$1")
    case "$pci" in ""|*,*) ;; *) d=$(ls "/sys/bus/pci/devices/$pci/infiniband" 2>/dev/null) ;; esac
  fi
  case "$d" in ""|*[!A-Za-z0-9_-]*) echo "" ;; *) echo "$d" ;; esac
}`
}

// gidDiscoveryFunc returns the bash function for RoCEv2 GID auto-discovery.
// Scans sysfs GID table for a RoCE v2 entry. Prefers IPv4-mapped addresses
// (ffff: prefix) but falls back to any RoCE v2 GID. Uses continue (not break)
// on empty entries because some systems have sparse GID tables (e.g. entries
// 0-3 empty, first RoCE v2 at index 5).
func gidDiscoveryFunc() string {
	return `find_rocev2_gid() {
  local dev=$1
  local types_dir="/sys/class/infiniband/$dev/ports/1/gid_attrs/types"
  [ -d "$types_dir" ] || { echo "-1"; return 1; }
  local max_gid=$(ls "$types_dir" 2>/dev/null | sort -n | tail -1)
  [ -z "$max_gid" ] && { echo "-1"; return 1; }
  for i in $(seq 0 $max_gid); do
    gtype=$(cat "$types_dir/$i" 2>/dev/null)
    [ -z "$gtype" ] && continue
    if [ "$gtype" = "RoCE v2" ]; then
      gid=$(cat /sys/class/infiniband/$dev/ports/1/gids/$i 2>/dev/null)
      if echo "$gid" | grep -q "0000:0000:0000:0000:0000:ffff:"; then
        echo "$i"; return 0
      fi
    fi
  done
  for i in $(seq 0 $max_gid); do
    gtype=$(cat "$types_dir/$i" 2>/dev/null)
    [ -z "$gtype" ] && continue
    if [ "$gtype" = "RoCE v2" ]; then
      echo "$i"; return 0
    fi
  done
  echo "-1"; return 1
}`
}

// ibvGIDFlagExpr returns the bash expression for ibv_rc_pingpong's -g flag.
// For RoCE with auto-discover: uses find_rocev2_gid with validation
// For RoCE with fixed index:   "-g N"
// For IB:                      "" (empty, no flag)
func (j *PingMeshJob) ibvGIDFlagExpr(devVar string) string {
	if j.RDMAType != config.RDMATypeRoCE {
		return ""
	}
	if j.GIDIndex >= 0 {
		return fmt.Sprintf(" -g %d", j.GIDIndex)
	}
	return fmt.Sprintf(" -g $(find_rocev2_gid %s)", devVar)
}

func (j *PingMeshJob) ibvNeedsGIDDiscovery() bool {
	return j.RDMAType == config.RDMATypeRoCE && j.GIDIndex < 0
}

func (j *PingMeshJob) serverScript() []string {
	if j.RDMAType == config.RDMATypeSRD {
		return j.srdServerScript()
	}
	return j.ibvServerScript()
}

func (j *PingMeshJob) clientScript(serverIP string) []string {
	if j.RDMAType == config.RDMATypeSRD {
		return j.srdClientScript(serverIP)
	}
	return j.ibvClientScript(serverIP)
}

// pingEndpoint is one side of a probe: a fixed device name, or a rail whose device
// the pod resolves at runtime.
type pingEndpoint struct {
	dev  string // shell expression for the device
	rail string
}

func (j *PingMeshJob) endpoints(devs []string) []pingEndpoint {
	var eps []pingEndpoint
	if len(j.Rails) > 0 {
		for _, r := range j.Rails {
			eps = append(eps, pingEndpoint{dev: "$(resolve_dev " + railEnvKey(r) + ")", rail: r})
		}
		return eps
	}
	for _, d := range devs {
		if checks.ValidDeviceName.MatchString(d) {
			eps = append(eps, pingEndpoint{dev: d})
		}
	}
	return eps
}

func (j *PingMeshJob) writeHelpers(sb *strings.Builder) {
	if j.ibvNeedsGIDDiscovery() {
		sb.WriteString(gidDiscoveryFunc() + "\nexport -f find_rocev2_gid\n\n")
	}
	if len(j.Rails) > 0 {
		sb.WriteString(resolveDevFunc() + "\nexport -f resolve_dev\n\n")
	}
}

func (j *PingMeshJob) ibvServerScript() []string {
	var sb strings.Builder
	sb.WriteString("#!/bin/bash\nmkdir -p /tmp\nexec 2>/tmp/pm_server_err.log\n\n")
	j.writeHelpers(&sb)

	// An unresolved rail still uses its port slots so client port numbers stay aligned.
	fmt.Fprintf(&sb, "timeout %d bash -c '\nidx=0\n", j.serverTimeout())
	for _, s := range j.endpoints(j.ServerDevices) {
		gidFlag := j.ibvGIDFlagExpr("$sdev")
		fmt.Fprintf(&sb, "sdev=%s\n", s.dev)
		fmt.Fprintf(&sb, "for cslot in $(seq 0 %d); do\n", len(j.endpoints(j.ClientDevices))-1)
		fmt.Fprintf(&sb, "  [ -n \"$sdev\" ] && ibv_rc_pingpong -d $sdev%s -p $((18515 + idx)) -n %d > /dev/null 2>&1 &\n",
			gidFlag, j.Iterations)
		sb.WriteString("  idx=$((idx + 1))\ndone\n")
	}
	sb.WriteString("wait\n' > /dev/null 2>&1 || true\n")

	return []string{"bash", "-c", sb.String()}
}

func (j *PingMeshJob) srdServerScript() []string {
	var sb strings.Builder
	sb.WriteString("#!/bin/bash\nmkdir -p /tmp\nexec 2>/tmp/pm_server_err.log\n\n")

	fmt.Fprintf(&sb, "timeout %d bash -c '\nidx=0\n", j.serverTimeout())
	for _, sdev := range j.ServerDevices {
		if !checks.ValidDeviceName.MatchString(sdev) {
			continue
		}
		fmt.Fprintf(&sb, "sdev=%s\n", sdev)
		fmt.Fprintf(&sb, "for cslot in $(seq 0 %d); do\n", len(j.ClientDevices)-1)
		// fi_rdm_pingpong: -p is provider, -E is OOB TCP port for address exchange.
		// FI_EFA_IFACE selects the server NIC (fabtests -d is domain name, not device).
		fmt.Fprintf(&sb, "  FI_EFA_IFACE=$sdev fi_rdm_pingpong -p efa -E=$((%d + idx)) -S %d -I %d > /dev/null 2>&1 &\n",
			pingmeshPortBase, srdPingMessageSize, j.Iterations)
		sb.WriteString("  idx=$((idx + 1))\ndone\n")
	}
	sb.WriteString("wait\n' > /dev/null 2>&1 || true\n")

	return []string{"bash", "-c", sb.String()}
}

func (j *PingMeshJob) ibvClientScript(serverIP string) []string {
	var sb strings.Builder
	sb.WriteString("#!/bin/bash\nmkdir -p /tmp/pm\nexec 2>/tmp/pm/script_stderr.log\n\n")
	j.writeHelpers(&sb)

	// Same endpoint order as the server keeps ports aligned. Results are cdev:sdev:rc:crail:srail;
	// in rail mode the server's device is unknown here, so sdev is empty and rails identify the pair.
	sb.WriteString("idx=0\n")
	for _, s := range j.endpoints(j.ServerDevices) {
		for _, c := range j.endpoints(j.ClientDevices) {
			sdev := s.dev
			if s.rail != "" {
				sdev = ""
			}
			result := fmt.Sprintf("echo \"$cdev:%s:$rc:%s:%s\" >> /tmp/pm/results.txt\n", sdev, c.rail, s.rail)
			fmt.Fprintf(&sb, "cdev=%s\n", c.dev)
			fmt.Fprintf(&sb, "if [ -z \"$cdev\" ]; then\n  echo 'no RDMA device for %s in this pod' > /tmp/pm/out_${idx}.txt; rc=1\n", c.rail)
			if j.ibvNeedsGIDDiscovery() {
				sb.WriteString("elif ! _gid=$(find_rocev2_gid \"$cdev\"); then\n")
				sb.WriteString("  echo \"no RoCE v2 GID for $cdev\" > /tmp/pm/out_${idx}.txt; rc=1\n")
				fmt.Fprintf(&sb,
					"else\n  timeout %d ibv_rc_pingpong -d \"$cdev\" -g $_gid -p $((18515 + idx)) -n %d %s > /tmp/pm/out_${idx}.txt 2>&1; rc=$?\n",
					j.Timeout, j.Iterations, serverIP)
			} else {
				fmt.Fprintf(&sb,
					"else\n  timeout %d ibv_rc_pingpong -d \"$cdev\"%s -p $((18515 + idx)) -n %d %s > /tmp/pm/out_${idx}.txt 2>&1; rc=$?\n",
					j.Timeout, j.ibvGIDFlagExpr("\"$cdev\""), j.Iterations, serverIP)
			}
			sb.WriteString("fi\n" + result + "idx=$((idx + 1))\n")
		}
	}

	j.appendClientJSON(&sb)
	return []string{"bash", "-c", sb.String()}
}

// srdClientScript generates the EFA/SRD client script. Unlike ibvClientScript,
// this uses bash loops over device arrays rather than unrolling every server×client
// pair inline: a 32×32 mesh produced a ~293KB script embedded in bash -c, which
// exceeded ARG_MAX ("argument list too long") and the pod exited before emitting JSON.
// TODO: ibvClientScript has the same unrolled pattern and may need the loop approach
// if large-NIC IB/RoCE clusters hit ARG_MAX.
func (j *PingMeshJob) srdClientScript(serverIP string) []string {
	var sb strings.Builder
	sb.WriteString("#!/bin/bash\nmkdir -p /tmp/pm\nexec 2>/tmp/pm/script_stderr.log\n\n")

	sdevs := j.ServerDevices
	cdevs := j.ClientDevices
	sb.WriteString(bashQuotedArray("SDEVS", sdevs))
	sb.WriteString("\n")
	sb.WriteString(bashQuotedArray("CDEVS", cdevs))
	sb.WriteString("\n\n")

	// Port indices must match srdServerScript (same ServerDevices × ClientDevices order).
	sb.WriteString("idx=0\nfor sdev in \"${SDEVS[@]}\"; do\n  for cdev in \"${CDEVS[@]}\"; do\n")
	fmt.Fprintf(&sb,
		"    (timeout %d env FI_EFA_IFACE=$cdev fi_rdm_pingpong -p efa -E=$((%d + idx)) -S %d -I %d %s > /tmp/pm/out_${idx}.txt 2>&1; echo $? > /tmp/pm/rc_${idx}.txt) &\n",
		j.Timeout, pingmeshPortBase, srdPingMessageSize, j.Iterations, serverIP,
	)
	sb.WriteString(`    idx=$((idx + 1))
  done
done
wait
idx=0
for sdev in "${SDEVS[@]}"; do
  for cdev in "${CDEVS[@]}"; do
    echo "${cdev}:${sdev}:$(cat /tmp/pm/rc_${idx}.txt)" >> /tmp/pm/results.txt
    idx=$((idx + 1))
  done
done
`)

	j.appendClientJSON(&sb)
	return []string{"bash", "-c", sb.String()}
}

func (j *PingMeshJob) appendClientJSON(sb *strings.Builder) {
	fmt.Fprintf(sb, `
printf '{"server_node":"%s","client_node":"%s","results":['
first=1
idx=0
while IFS=: read -r cdev sdev rc crail srail; do
  [ $first -eq 0 ] && printf ','
  first=0
  if [ "$rc" -eq 0 ]; then
    printf '{"src_dev":"%%s","dst_dev":"%%s","src_rail":"%%s","dst_rail":"%%s","pass":true}' "$cdev" "$sdev" "$crail" "$srail"
  else
    err=$(head -c 200 /tmp/pm/out_${idx}.txt 2>/dev/null | tr '"' "'" | tr '\\' '/' | tr '\n' ' ' | tr -d '\000-\037')
    printf '{"src_dev":"%%s","dst_dev":"%%s","src_rail":"%%s","dst_rail":"%%s","pass":false,"error":"%%s"}' "$cdev" "$sdev" "$crail" "$srail" "$err"
  fi
  idx=$((idx + 1))
done < /tmp/pm/results.txt
printf ']}'
`, j.ServerNode, j.ClientNode)
}

func (j *PingMeshJob) ServerSpec(node, namespace, image string) (*batchv1.Job, error) {
	return jobrunner.BuildJobSpec(j.Name(), node, namespace, image, jobrunner.RoleServer, j.PodCfg,
		j.serverScript())
}

func (j *PingMeshJob) ClientSpec(node, namespace, image, serverIP string) (*batchv1.Job, error) {
	return jobrunner.BuildJobSpec(j.Name(), node, namespace, image, jobrunner.RoleClient, j.PodCfg,
		j.clientScript(serverIP))
}

// pingmeshClientOutput is the wrapper JSON object emitted by client pods.
type pingmeshClientOutput struct {
	ServerNode string               `json:"server_node"`
	ClientNode string               `json:"client_node"`
	Results    []PingMeshPairResult `json:"results"`
}

// ParseResult parses the client pod JSON output into a JobResult.
func (j *PingMeshJob) ParseResult(logs string) (*jobrunner.JobResult, error) {
	// Defensive extraction: find the JSON object bounds
	start := strings.Index(logs, "{")
	end := strings.LastIndex(logs, "}")
	if start < 0 || end < 0 || end <= start {
		return nil, fmt.Errorf("no JSON object found in pingmesh client output")
	}
	jsonStr := logs[start : end+1]

	var output pingmeshClientOutput
	if err := json.Unmarshal([]byte(jsonStr), &output); err != nil {
		return nil, fmt.Errorf("failed to parse pingmesh JSON: %w", err)
	}
	results := output.Results

	passed := 0
	for _, r := range results {
		if r.Pass {
			passed++
		}
	}

	status := checks.StatusPass
	msg := fmt.Sprintf("Pingmesh: %d/%d NIC pairs passed", passed, len(results))
	if len(results) == 0 {
		status = checks.StatusFail
		msg = "Pingmesh: no NIC pairs probed"
	} else if passed == 0 {
		status = checks.StatusFail
	} else if passed < len(results) {
		status = checks.StatusFail
	}

	return &jobrunner.JobResult{
		Status:  status,
		Message: msg,
		Details: results,
	}, nil
}
