# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

kubectl plugin (`kubectl rhaii-validate`) for validating GPU cluster readiness for AI/ML workloads. Runs hardware and network checks on GPU nodes via privileged Kubernetes Jobs. Auto-detects GPU vendor (NVIDIA/AMD), platform (AKS/EKS/CoreWeave/OCP), and cluster topology.

Binary: `rhaii-validator`. kubectl plugin name: `kubectl-rhaii_validate`.

**Epic:** INFERENG-4707

## Build and Test Commands

```bash
make build              # CGO_ENABLED=0 go build → bin/rhaii-validator
make install            # Build + install as kubectl plugin to /usr/local/bin
make test               # go test ./... -v
make lint               # golangci-lint run ./...
make fmt                # go fmt ./...
make container          # Build validator container (Dockerfile.dev)
make container-rdma     # Build tools container (tools/Dockerfile.dev)
```

Run a single test:
```bash
go test ./pkg/checks/rdma/ -run TestParseLoopbackBWOutput -v
```

## Architecture

### Two Execution Modes

The binary runs in two modes controlled by subcommands:

1. **Controller mode** (`gpu`, `network`, `rdma`, `all`, etc.): Runs on the user's machine. Creates K8s namespace, RBAC, deploys Jobs to GPU nodes, collects results from pod logs (JSON), prints report, cleans up. Orchestrated by `pkg/controller/controller.go`.

2. **Agent mode** (`run` — hidden subcommand): Runs inside per-node Job pods. Executes checks directly (nvidia-smi injected by GPU runtime, sysfs read via privileged mode), outputs JSON report to stdout (progress to stderr). The controller parses JSON from pod logs. Orchestrated by `pkg/runner/runner.go`.

### Key Interfaces

- **`checks.Check`** (`pkg/checks/check.go`): Per-node check (gpu driver, ECC, RDMA devices, topology). Returns `checks.Result` with Status (PASS/WARN/FAIL/SKIP).

- **`jobrunner.Job`** (`pkg/jobrunner/job.go`): Multi-node test (iperf3, ib_write_bw, ibv_rc_pingpong). Produces server/client K8s Job specs, parses stdout into `JobResult`. Optional interfaces: `Configurable`, `ThresholdConfigurable`, `ImageConfigurable`, `NameSuffixable`.

### Controller Execution Flow (`controller.Run`)

1. Cleanup previous runs
2. Ensure namespace + RBAC (+ SCC on OpenShift)
3. Detect platform, create/load config ConfigMap
4. Tier 1: CRD + operator health checks (API-only, no pods)
5. Discover GPU nodes (label-based, fallback to allocatable scan)
6. Deploy per-node GPU check Jobs (request all GPUs for nvidia-smi)
7. Deploy per-node RDMA node check Jobs (topology, devices, NIC status)
   - If flat PCIe topology detected: run intra-host BW probe to find optimal GPU-NIC pairs
8. Run pingmesh RDMA connectivity mesh (pairwise ibv_rc_pingpong)
9. Run multi-node bandwidth jobs (ring/star topology via jobrunner)
10. Store JSON report in ConfigMap, print, cleanup

### Two Job Types

| | Per-node Jobs | Multi-node Jobs |
|---|---|---|
| Image | validator (rhaii-validator binary) | tools (iperf3/RDMA) or validator (tcp-lat) |
| GPU request | All GPUs (for nvidia-smi visibility) | 1 per pod |
| Host access | privileged (host sysfs visibility) | None |
| Framework | `pkg/runner` | `pkg/jobrunner` (ring/star/pairwise scheduling) |

### Container Images

Two images, resolved from `manifests/image-references/image-references.yaml` (embedded via `//go:embed`):
- **Validator** (`RELATED_IMAGE_RHAII_CLUSTER_VALIDATOR`): This binary. Used for per-node checks and TCP latency.
- **Tools** (`RELATED_IMAGE_RHAII_VALIDATOR_TOOLS`): iperf3, ib_write_bw, ibv_rc_pingpong for network/RDMA jobs.

Override with env vars or CLI flags (`--image`, `--tools-image`).

### Platform Config

Embedded per-platform YAML in `pkg/config/platforms/`. Only configurable values — everything else is auto-detected. Loaded at startup, can be overridden via ConfigMap (`rhaii-validate-config`) in the cluster. Key configurable fields: `gpu.min_driver_version`, `jobs.requests/limits` (including RDMA resources), `thresholds.*`, `jobs.rdma_type`.

### Report Storage

JSON report stored in ConfigMap `rhaii-validate-report`. Supports report merging: runs that don't produce certain sections (e.g., `rdma-ping` doesn't produce Nodes) preserve data from previous runs.

## CLI Subcommands

```
gpu              GPU hardware checks (driver, ECC)
network          TCP bandwidth + latency (iperf3, tcp-lat)
rdma             All RDMA: rdma-node + rdma-ping + rdma-bandwidth
rdma-node        Per-node RDMA checks (devices, status, topology)
rdma-ping        RDMA connectivity mesh (ibv_rc_pingpong)
rdma-bandwidth   RDMA bandwidth (ib_write_bw)
all              Everything (deps + gpu + network + rdma)
deps             CRDs + operator health (no pods deployed)
clean            Remove validation resources
run              (hidden) Agent mode for per-node Jobs
```

Common flags: `--debug`, `-o json`, `--image`, `--tools-image`, `--namespace`, `--nodes`, `--server-node`, `--pull-secret`.

## Coding Conventions

- Per-node checks implement `checks.Check`; multi-node tests implement `jobrunner.Job`
- GPU tools (nvidia-smi, rocm-smi) are injected by GPU container runtime via resource requests; RDMA tools (ibv_devices, ibstat) and sysfs reads use privileged mode's host visibility
- Use `apierrors.IsNotFound()` for K8s errors, never string matching
- Deploy manifests embedded via `//go:embed` (not read from filesystem)
- `run` subcommand is hidden and internal — only used by per-node Job pods
- Agent JSON report goes to stdout, progress to stderr — container runtimes merge both in `kubectl logs`, so the controller skips lines until it finds `{`
- RDMA bandwidth jobs are expanded per GPU-NIC pair from topology (not one job per node)
- Pingmesh uses N-choose-2 pairwise scheduling with round-robin tournament and 3 controller-managed retries
- GPU vendor is always auto-detected (never configured)
- RDMA resource (e.g., `rdma/ib`, `nvidia.com/roce`) must be manually configured in platform config

## Known Limitations

- AMD GPUs: bandwidth jobs skipped (tools image is NVIDIA-only)
- `ibv_devices`/`ibstat` not on AKS hosts — sysfs fallback used
- NCCL all-reduce job: framework ready, needs NCCL image
- `deps` subcommand: partially implemented (CRDs + operators)
