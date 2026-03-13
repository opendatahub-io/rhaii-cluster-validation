# RHAII Cluster Validation Agent

## Project Overview

This repo contains the **Tier 2 agent** for RHAII cluster validation. It runs as a privileged DaemonSet on GPU nodes to perform hardware-level checks that cannot be done via the Kubernetes API.

**Tier 1 (API checks)** will live in [odh-cli](https://github.com/opendatahub-io/odh-cli) (`kubectl odh validate`) — integration planned.
**Tier 2 (agent checks)** live here — this is the agent binary that runs on each GPU node.

The binary has **two modes**:
- **`run`** — Agent mode: runs hardware checks on the current node (used by DaemonSet pods)
- **`deploy`** — Controller mode: deploys agents to GPU nodes, collects results, prints report

**Phase 1 (now):** Standalone — `deploy` command handles the full lifecycle.
**Phase 2 (later):** Integrate with odh-cli — `pkg/controller/` and `pkg/checks/` imported as Go modules.

**Epic:** INFERENG-4707

## Architecture

```
rhaii-validate-agent deploy --image <img>  (controller mode, runs on workstation)
    |
    +-- Ensures namespace + RBAC (from embedded deploy/rbac.yaml)
    +-- Detects platform (AKS/EKS/CoreWeave/OCP) from node labels
    +-- Creates ConfigMap with platform defaults (preserved across runs)
    +-- Discovers GPU nodes via K8s API
    +-- Deploys DaemonSet (from embedded deploy/daemonset.yaml)
    |       |
    |       +-- rhaii-validate-agent run  (agent mode, on each GPU node)
    |       |       +-- Annotation: starting → running → done/error
    |       |       +-- GPU checks (nvidia-smi) — no GPU resource request needed
    |       |       +-- RDMA checks (ibstat, ibv_devices)
    |       |       +-- JSON to stdout, blocks on <-ctx.Done()
    |       |
    +-- Waits for "done"/"error" annotations on all pods
    +-- Collects results from pod logs
    |
    +-- [auto, 2+ GPU nodes] Job Runner Framework
    |       +-- Auto-detects GPU resource (nvidia.com/gpu or amd.com/gpu)
    |       +-- Auto-detects GPU count per node from node.Status.Allocatable
    |       +-- Deploys server Job on one node (iperf3 -s)
    |       +-- Deploys client Jobs on other nodes (iperf3 -c <serverIP>)
    |       +-- Jobs request all available GPUs (auto-detected)
    |       +-- Collects results, parses output
    |       +-- Cleans up Jobs automatically
    |
    +-- Cleans up DaemonSet + RBAC (ConfigMap preserved for reuse)
    +-- Prints consolidated report
    +-- Exits non-zero if any check reported FAIL
```

## Two Workload Types

| | DaemonSet Agent | Job Runner |
|---|---|---|
| **Purpose** | Per-node hardware checks | Multi-node bandwidth/NCCL tests |
| **GPU request** | None (privileged mode, nvidia-smi only) | All GPUs auto-detected from node |
| **K8s resource** | DaemonSet (one pod per GPU node) | Jobs (server + clients) |
| **Completion** | Annotation-based (`done`/`error`) | Job status (succeeded/failed) |
| **Triggered by** | Always (part of deploy) | Automatic when 2+ GPU nodes and jobs registered |

## Language and Framework

- **Language:** Go 1.25+
- **CLI framework:** Cobra (two subcommands: `run`, `deploy`)
- **K8s client:** client-go (controller deploys DaemonSet + Jobs, collects pod logs)
- **Container base:** UBI9 (`registry.access.redhat.com/ubi9/ubi-minimal`)
- **Container image:** Single image for both agent and jobs (includes iperf3, perftest, rdma tools)
- **Version:** Set at build time via `-ldflags "-X main.version=..."`, defaults to `"dev"`

## CLI Usage

```bash
# Version
rhaii-validate-agent --version

# Agent mode — runs checks on current node
rhaii-validate-agent run                                    # Auto-detect node name
rhaii-validate-agent run --no-wait                          # Exit immediately (local/CI)
rhaii-validate-agent run --bandwidth --iperf-server <ip>    # + TCP bandwidth test (standalone mode)
rhaii-validate-agent run --config config.yaml               # Override auto-loaded config

# Controller mode — deploy agents, collect results, print report
rhaii-validate-agent deploy --image <img>                   # Full lifecycle
rhaii-validate-agent deploy --image <img> --server-node <node>  # Specific server node for jobs
rhaii-validate-agent deploy --image <img> --client-nodes <n1,n2>  # Specific client nodes for jobs

# Makefile shortcuts
make deploy IMG=<img>                                       # Build + deploy + collect + report
make run-local                                              # Run checks locally with --no-wait
```

## Exit Codes

- **0** — all checks passed (or passed with warnings)
- **1** — one or more checks reported FAIL (CI/CD can gate on this)

## Project Structure

```
rhaii-cluster-validation/
├── cmd/agent/
│   └── main.go                     # CLI: run + deploy subcommands, version via ldflags
│
├── pkg/
│   ├── annotator/                  # Pod annotation updates (starting/running/done/error)
│   │   ├── annotator.go            # NewWithClient() for DI, SetStatus() via JSON merge patch
│   │   └── annotator_test.go
│   │
│   ├── checks/                     # Per-node checks + multi-node jobs (by category)
│   │   ├── check.go                # Check interface, Result, NodeReport types
│   │   ├── gpu/                    # driver.go, ecc.go + tests
│   │   ├── rdma/                   # devices.go, status.go + rdmabw_job.go + tests
│   │   └── networking/             # bandwidth.go + iperf_job.go + tests
│   │
│   ├── config/                     # Platform detection + config loading
│   │   ├── platform.go             # PlatformConfig, PodConfiguration structs
│   │   ├── detect.go               # Auto-detect from node labels/provider ID
│   │   ├── loader.go               # Load embedded defaults + override file
│   │   └── platforms/*.yaml        # Embedded per-platform configs
│   │
│   ├── controller/                 # Full lifecycle orchestration
│   │   ├── controller.go           # RBAC → config → DaemonSet → wait → collect → jobs → cleanup
│   │   └── controller_test.go
│   │
│   ├── jobrunner/                  # Generic multi-node job framework
│   │   ├── job.go                  # Job interface, Configurable, PodConfig, JobResult types
│   │   ├── runner.go               # RunJob: server → wait IP → clients → wait → collect → cleanup
│   │   └── helpers.go              # BuildJobSpec: base K8s Job with PodConfig applied
│   │
│   └── runner/                     # Check execution engine
│       ├── runner.go               # Run checks, output JSON, return report
│       └── runner_test.go
│
├── deploy/
│   ├── embed.go                    # //go:embed for daemonset.yaml and rbac.yaml
│   ├── daemonset.yaml              # Agent DaemonSet (single source of truth)
│   └── rbac.yaml                   # SA + ClusterRole + ClusterRoleBinding
│
├── examples/configmap.yaml         # Example ConfigMap for customization
├── docs/dev.md                     # odh-cli integration guide
├── Dockerfile                      # Multi-stage UBI9 build
├── Makefile                        # build, test, container, deploy, run-local
└── .gitignore
```

## Job Runner Framework

### Job Interface

```go
type Job interface {
    Name() string
    ServerSpec(node, namespace, image string) *batchv1.Job
    ClientSpec(node, namespace, image, serverIP string) *batchv1.Job
    ParseResult(logs string) (*JobResult, error)
}
```

### GPU Auto-Detection

The controller auto-detects GPU resources from `node.Status.Allocatable`:
- Scans for `nvidia.com/gpu` and `amd.com/gpu` extended resources
- Takes minimum count across all GPU nodes
- Sets both requests and limits on job pods for exclusive GPU access
- No manual configuration needed — fully automated from cluster state

### PodConfig

Jobs accept a `PodConfig` for resource requests, limits, and annotations:

```go
type PodConfig struct {
    Annotations      map[string]string  // arbitrary pod annotations
    ResourceRequests map[string]string  // e.g. {"nvidia.com/gpu": "8", "cpu": "500m"}
    ResourceLimits   map[string]string  // e.g. {"nvidia.com/gpu": "8"}
    Privileged       bool
}
```

### Built-in Jobs

| Job | Server | Client | Output |
|---|---|---|---|
| `iperf3-tcp` | `iperf3 -s --one-off` | `iperf3 -c <ip> -t 10 -J` | JSON (bandwidth Gbps) |
| `ib-write-bw` | `ib_write_bw --duration 10` | `ib_write_bw --duration 10 <ip>` | Text (bandwidth Gbps) |

### Lifecycle

```
1. Create server Job (nodeSelector → server node)
2. Wait for server pod Running + PodIP
3. Create client Jobs on each client node with server IP
4. Wait for all client Jobs to complete
5. Collect logs, parse via job.ParseResult()
6. Cleanup all Jobs (foreground deletion)
7. Return []JobResult
```

## Pod Annotation Status Tracking

Agent updates `rhaii.opendatahub.io/validation-status`:

```
starting → running → done   (checks completed)
                   → error  (agent itself failed)
```

## Platform Configuration

### Config Persistence

ConfigMap `rhaii-validate-config` is **preserved across runs** — not deleted during cleanup. Users can:
1. Run deploy → ConfigMap created with detected defaults
2. Edit: `kubectl edit configmap rhaii-validate-config -n rhaii-validation`
3. Rerun deploy → existing ConfigMap used, overrides merged on top of defaults

### PodConfiguration in Platform Config

`podConfiguration` in the platform config controls pod-level settings. For the DaemonSet agent, resource requests are applied from the config. For jobs, GPU resources are auto-detected.

```yaml
podConfiguration:
  annotations:
    custom-annotation: "value"
  resourceRequests:
    cpu: "500m"
    memory: "512Mi"
  resourceLimits:
    memory: "1Gi"
```

## Build and Test

```bash
make build          # Build binary (version from git describe)
make test           # Run unit tests
make container      # Build container image (IMG=...)
make push           # Push container image (IMG=...)
make deploy         # Full lifecycle: build + deploy + collect + report (IMG=...)
make run            # Deploy DaemonSet only via kubectl (IMG=...)
make logs           # Collect results from pod logs
make clean          # Remove DaemonSet
make run-local      # Run locally with --no-wait
make fmt / lint     # Format / lint
```

## Coding Conventions

- All checks implement the `Check` interface
- External tool output parsing extracted into testable functions
- `apierrors.IsNotFound()` / `apierrors.IsAlreadyExists()` for K8s errors (not string matching)
- Deploy manifests embedded via `//go:embed` (single source of truth)
- `NewWithClient()` constructors for dependency injection in tests
- Error messages lowercase per Go convention
- `SilenceUsage: true` on cobra — no flag dump on errors

## Known TODOs

1. **nvidia-smi path from config:** `GPU.NvidiaSmiPath` exists in config but volume mounts hardcode `/usr/bin`
2. **NCCL all-reduce job:** Requires NCCL libraries in container image + GPU resource requests (framework ready, implementation needed)
3. **Unit tests for jobrunner:** Test job spec generation and result parsing with fake clientset
