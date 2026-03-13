# RHAII Cluster Validation Agent

Hardware validation tool for GPU, RDMA, and network checks on Kubernetes clusters. Validates cluster readiness for llm-d / RHAII inference workloads.

Two modes:
- **`run`** — Agent mode: runs hardware checks on the current node
- **`deploy`** — Controller mode: deploys agents to all GPU nodes, collects results, prints report

## What It Checks

### Per-Node Checks (DaemonSet agent — no GPU request needed)

| Category | Check | Tool |
|---|---|---|
| GPU | Driver version, CUDA version | nvidia-smi |
| GPU | ECC memory errors | nvidia-smi |
| RDMA | Device presence and accessibility | ibv_devices, /dev/infiniband |
| RDMA | NIC link state and speed | ibstat |

### Cross-Node Bandwidth Tests (Jobs — auto when 2+ GPU nodes, requests all available GPUs)

| Category | Check | Tool |
|---|---|---|
| Networking | TCP bandwidth | iperf3 |
| Networking | RDMA bandwidth | ib_write_bw |

## Quick Start

### Build

```bash
make build
```

### Run locally (no GPU expected, checks fail gracefully)

```bash
make run-local
```

### Deploy to cluster

Build and push the container image first:

```bash
make container IMG=quay.io/{user}/rhaii-validate-agent:dev
make push IMG=quay.io/{user}/rhaii-validate-agent:dev
```

**Workflow 1: make deploy (recommended)**

Single command — builds binary, creates RBAC, deploys agents, collects results, prints report, cleans up:

```bash
make deploy IMG=quay.io/{user}/rhaii-validate-agent:dev
```

With specific server/client topology for bandwidth jobs:

```bash
./bin/rhaii-validate-agent deploy --image <img> \
  --server-node gpu-node-0 --client-nodes gpu-node-1,gpu-node-2
```

Multi-node bandwidth jobs (iperf3, ib_write_bw) run automatically when 2+ GPU nodes exist. GPU resources are auto-detected.

**Workflow 2: Makefile manual (for debugging)**

```bash
kubectl apply -f deploy/rbac.yaml                          # Create RBAC first
make run IMG=quay.io/{user}/rhaii-validate-agent:dev        # Deploy DaemonSet only
make logs                                                   # Manually read pod logs
make clean                                                  # Manually cleanup DaemonSet
```

## CLI Usage

### Version

```bash
rhaii-validate-agent --version
```

### Agent mode — run checks on current node

```bash
rhaii-validate-agent run                                     # Auto-detect node name
rhaii-validate-agent run --no-wait                           # Exit after checks (for local/CI)
rhaii-validate-agent run --bandwidth --iperf-server <ip>     # + TCP bandwidth test (standalone)
rhaii-validate-agent run --config config.yaml                # Override platform defaults
```

Without `--no-wait`, the agent blocks after completing checks (required for DaemonSet — keeps the container alive so the controller can collect logs). With `--no-wait`, the agent exits immediately and returns non-zero if any check failed.

### Controller mode — deploy agents, collect results, print report

```bash
rhaii-validate-agent deploy --image <img>                    # Full lifecycle
rhaii-validate-agent deploy --image <img> --server-node <node>  # Specific server node
rhaii-validate-agent deploy --image <img> --client-nodes <n1,n2>  # Specific clients
rhaii-validate-agent deploy --image <img> --namespace my-ns --timeout 10m
rhaii-validate-agent deploy --image <img> --config config.yaml
```

The `deploy` command:
1. Cleans up any previous DaemonSet
2. Ensures namespace and RBAC exist
3. Detects the cloud platform (AKS/EKS/CoreWeave/OCP) from node labels
4. Creates a ConfigMap with detected platform defaults (preserved across runs)
5. Discovers GPU nodes and deploys agent DaemonSet
6. Waits for agents to set `validation-status: done` annotation
7. Collects JSON results from pod logs
8. If `--bandwidth`: runs bandwidth jobs (iperf3/RDMA) between GPU nodes
   - Auto-detects GPU resource name (`nvidia.com/gpu` or `amd.com/gpu`) and count
   - Jobs request all available GPUs for exclusive access
   - Default: first GPU node as server, rest as clients
9. Cleans up DaemonSet and RBAC (ConfigMap preserved for reuse)
10. Prints consolidated report — exits non-zero if any check reported FAIL

## GPU Resource Handling

| Workload | GPU Request | Why |
|---|---|---|
| DaemonSet agent | None | Only queries nvidia-smi in privileged mode |
| Bandwidth/NCCL jobs | All available GPUs | Auto-detected from `node.Status.Allocatable`, requests all for exclusive access |

The controller scans nodes for `nvidia.com/gpu` or `amd.com/gpu` extended resources and automatically sets resource requests/limits on job pods. No manual configuration needed.

## Exit Codes

| Code | Meaning |
|---|---|
| 0 | All checks passed (or passed with warnings) |
| 1 | One or more checks reported FAIL |

Both `run --no-wait` and `deploy` exit non-zero on failures, allowing CI/CD pipelines to gate on results.

## Pod Annotation Status Tracking

The agent updates its own pod's annotation `rhaii.opendatahub.io/validation-status`:

```
starting  →  running  →  done    (checks completed successfully)
                      →  error   (agent itself failed)
```

The controller waits for all agent pods to reach `done` or `error` before collecting logs. The agent blocks after setting the annotation so the container stays alive for log collection.

## Output

### Agent mode — JSON to stdout

```json
{
  "node": "aks-gpuh100-vmss000000",
  "timestamp": "2026-03-12T14:30:00Z",
  "results": [
    {
      "category": "gpu_hardware",
      "name": "gpu_driver_version",
      "status": "PASS",
      "message": "NVIDIA driver: 535.129.03, CUDA: 12.2, GPU: NVIDIA H100 80GB (81920 MiB), 8 GPU(s)"
    }
  ]
}
```

### Controller mode — table report

```
=== Validation Report ===
Platform: AKS

GROUP                CHECK                          NODE                                STATUS   MESSAGE
----------------------------------------------------------------------------------------------------------------------------------
gpu_hardware         gpu_driver_version             aks-gpuh100-vmss000000              PASS     NVIDIA driver: 535.129.03
gpu_hardware         gpu_ecc_status                 aks-gpuh100-vmss000000              PASS     No uncorrectable ECC errors
networking_rdma      rdma_devices_detected           aks-gpuh100-vmss000000              PASS     4 RDMA device(s) found
bandwidth            iperf3-tcp                     aks-gpuh100-vmss000001              PASS     TCP bandwidth: 98.5 Gbps (threshold: 25 Gbps)

Summary: 4 PASS | 0 WARN | 0 FAIL | 0 SKIP
Status:  READY
```

## Platform Configuration

### Config Persistence

ConfigMap `rhaii-validate-config` is **preserved across runs**. Edit and rerun without losing customizations:

```bash
# First run creates ConfigMap with detected defaults
make deploy IMG=<img>

# Edit config
kubectl edit configmap rhaii-validate-config -n rhaii-validation

# Rerun — uses your customized config
make deploy IMG=<img>
```

### How config loading works

```
Auto-detect platform (AKS/EKS/CoreWeave/OCP)
    |
    v
Load embedded defaults (pkg/config/platforms/aks.yaml)
    |
    v
Override with /etc/rhaii-validate/platform.yaml (if exists)
    |
    v
Override with --config flag (if provided, highest priority)
```

### Platform-specific defaults

| Setting | AKS | CoreWeave | EKS | OCP |
|---|---|---|---|---|
| GPU taint key | `sku` | `nvidia.com/gpu` | `nvidia.com/gpu` | `nvidia.com/gpu` |
| RDMA device plugin | `rdma-shared-device-plugin` | `rdma-shared-device-plugin` | `efa-device-plugin` | `rdma-shared-device-plugin` |
| NIC prefix | `mlx5_` | `mlx5_` | `efa_` | `mlx5_` |

### Example override

```yaml
# Only specify fields you want to change
gpu:
  min_driver_version: "550.0"
  supported_types: ["H200", "B200"]
thresholds:
  tcp_bandwidth_gbps:
    pass: 50
podConfiguration:
  annotations:
    custom-annotation: "value"
  resourceRequests:
    cpu: "1"
    memory: "2Gi"
```

See [examples/configmap.yaml](examples/configmap.yaml) for a full example.

## Architecture

```
rhaii-validate-agent deploy --image <img> --bandwidth
    |
    +-- RBAC + Namespace (from embedded deploy/rbac.yaml)
    +-- Platform detection + ConfigMap (preserved across runs)
    +-- DaemonSet agent (per-node checks, no GPU request)
    |       +-- GPU checks (nvidia-smi, privileged mode)
    |       +-- RDMA checks (ibv_devices, ibstat)
    |       +-- Annotation: starting → running → done/error
    |       +-- Blocks until controller collects logs
    |
    +-- Job Runner (multi-node bandwidth tests)
    |       +-- Auto-detect: nvidia.com/gpu=8 from node allocatable
    |       +-- Server Job on node A (iperf3 -s)
    |       +-- Client Jobs on nodes B,C (iperf3 -c <serverIP>)
    |       +-- All GPUs requested for exclusive access
    |
    +-- Collect all results → consolidated report
    +-- Cleanup (DaemonSet + RBAC, ConfigMap preserved)
```

## Testing

```bash
make test                                                    # Unit tests
make run-local                                               # Local checks (--no-wait)
make deploy IMG=quay.io/{user}/rhaii-validate-agent:dev      # Full cluster test
```

## Make Targets

```
make build          Build agent binary (version from git describe)
make test           Run unit tests
make container      Build container image (IMG=...)
make push           Push container image (IMG=...)
make deploy         Full lifecycle: build, deploy, collect, report, cleanup (IMG=...)
make run            Deploy agent DaemonSet only via kubectl (IMG=...)
make logs           Collect results from pod logs
make clean          Remove agent DaemonSet
make run-local      Run agent locally (--no-wait, exits after checks)
make fmt            Format code
make lint           Run linter
```
