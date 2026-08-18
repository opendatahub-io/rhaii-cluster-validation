# Architecture

This document describes the structure of `rhaii-cluster-validation` after the
controller-simplification refactor (Epic INFERENG-4707). It's meant as a map
of "what lives where and why," not a full API reference — see package doc
comments and `CLAUDE.md` for day-to-day development notes.

## Overview

`rhaii-validator` (kubectl plugin name `kubectl-rhaii_validate`) validates
that a Kubernetes cluster's GPU nodes are ready for AI/ML workloads: GPU
driver/ECC health, RDMA device/topology/connectivity, and TCP/RDMA bandwidth
between nodes. It auto-detects GPU vendor (NVIDIA/AMD), cloud platform
(AKS/EKS/CoreWeave/OCP), and cluster topology, then runs the appropriate
checks.

The binary runs in two modes:

1. **Controller mode** (`gpu`, `network`, `rdma`, `all`, `deps`, `clean`) —
   runs on the operator's machine (or, via the Helm chart, as an in-cluster
   Job). Creates a namespace + RBAC, deploys per-node and multi-node Jobs,
   collects results, prints/stores a report, cleans up.
2. **Agent mode** (`run` — hidden) — runs inside per-node Jobs. Executes
   checks directly against the host (nvidia-smi/rocm-smi injected by the GPU
   runtime, sysfs read via privileged mode) and prints a JSON report to
   stdout.

## Package layout

```
cmd/agent/            CLI entry point (cobra commands), both controller and agent subcommands
pkg/controller/        Orchestration: the validation lifecycle (thin — see below)
pkg/runner/            Agent-side: runs a set of checks.Check on one node, emits NodeReport
pkg/jobrunner/         Multi-node Job orchestration: ring/star/pairwise scheduling,
                        shared Job-spec builder (BuildJobSpec, ApplyResourceConfig, SetGPUResource)
pkg/checks/            Check interface + Result/NodeReport types, shared across all checks
pkg/checks/gpu/        Per-node GPU driver/ECC checks (NVIDIA + AMD)
pkg/checks/rdma/       RDMA: device/status/topology checks, GPU-NIC pairing algorithms,
                        pingmesh job + classification, RDMA bandwidth jobs
pkg/checks/networking/ TCP bandwidth (iperf3) and latency jobs/checks
pkg/checks/crd/        Tier-1 CRD presence/version checks (API-only, no pods)
pkg/checks/operator/   Tier-1 operator health checks (API-only, no pods)
pkg/config/            Platform detection, embedded per-platform YAML defaults,
                        GPU vendor/resource-name mapping, image resolution
pkg/report/            Report aggregation (status counting, readiness) and
                        table/JSON formatting — no Kubernetes dependency
deploy/                Embedded manifests (RBAC, per-node check Job template)
                        + the Go code that applies them (RBAC/SCC bootstrap)
manifests/             Embedded image-references.yaml (validator + tools image pins)
charts/                Helm chart for deploying the controller as an in-cluster Job
```

The dependency direction is roughly:

```
cmd/agent  →  controller  →  {jobrunner, checks/*, config, report, deploy}
                                    ↑
                             jobrunner ← checks/rdma, checks/networking (Job implementations)
```

`checks/rdma` and `checks/networking` implement `jobrunner.Job` for their
multi-node tests, so they depend on `jobrunner`, but `jobrunner` never depends
back on them — no cycle.

## Controller responsibilities

The controller (`pkg/controller/controller.go`) orchestrates `Run()`:

1. Clean up leftover resources from a previous run.
2. Ensure namespace + RBAC (`deploy.EnsureRBAC`, `deploy.EnsureOpenShiftSCC` on OCP).
3. Detect platform (`config.DetectPlatform`) and load/store the platform config ConfigMap.
4. Tier 1: CRD + operator health checks (API-only).
5. Discover GPU nodes (label-based, falls back to allocatable-resource scan).
6. Deploy per-node GPU-check and RDMA-node-check Jobs (shared builder:
   `deployNodeCheckJobs`), wait, collect JSON reports from pod logs.
7. If flat PCIe topology is detected, run an intra-host bandwidth probe to
   find optimal GPU-NIC pairs (`rdma.ApplyBandwidthPairing`).
8. Run the pingmesh RDMA connectivity mesh (pairwise `ibv_rc_pingpong`),
   classify results via `rdma.ClassifyPingMeshResults`.
9. Run multi-node bandwidth jobs (`jobrunner.Runner`, ring/star topology).
10. Aggregate + store the JSON report (`pkg/report`) in a ConfigMap, print it, clean up.

What the controller intentionally does **not** contain:

- **Job spec construction details** — shared by `jobrunner.BuildJobSpec` /
  `ApplyResourceConfig` / `SetGPUResource`, used by every Job type (per-node
  check Jobs, iperf3, RDMA bandwidth, pingmesh).
- **RBAC/SCC bootstrap** — `deploy.EnsureRBAC`, `deploy.EnsurePullSecret`,
  `deploy.EnsureOpenShiftSCC`. Generic manifest-apply logic, not
  validation-specific.
- **Pingmesh rail/cross-rail classification** — `rdma.ClassifyPingMeshResults`
  and friends. Pure data transformation (job results + topology in, a report
  out); fully unit-testable without a cluster.
- **Topology reconciliation** (BW-probe pairing, flat-topology warnings,
  topology-map building) — `rdma` package, next to the pairing algorithms
  that already lived there.
- **Report formatting/status math** — `pkg/report`. Pure formatting; no
  Kubernetes dependency.
- **GPU vendor/resource-name mapping** — `config.GPUNodeSelectors`,
  `config.GPUResourceForVendor`, etc.

What's left in the controller is genuinely orchestration: sequencing steps,
deciding which phases run for which `--check-mode`, wiring outputs of one
phase into inputs of the next, and the handful of Job-polling/log-collection
loops that are specific to how this tool structures its Jobs.

## Platform config

Platform differences (resource requests/limits, thresholds, RDMA type, CRD
minimum versions, operator namespaces) are data, not code — embedded YAML in
`pkg/config/platforms/{aks,eks,coreweave,ocp}.yaml`, loaded into a single
`config.PlatformConfig` struct at startup. Users can override any field via a
`rhaii-validate-config` ConfigMap or `--config` file. The controller and
Job-builders consume `PlatformConfig` fields generically — they never branch
on "if AKS then... if EKS then...". This is deliberately **not** a
plugin/strategy-per-platform system: one config struct, one loader, per-file
YAML data.

## Deployment model

Two ways to run validation:

1. **kubectl plugin (existing, primary)**: `kubectl rhaii-validate all`. Runs
   the controller locally against your kubeconfig; it self-provisions
   everything it needs (namespace, RBAC, Jobs) and tears them down afterward.
2. **Helm chart (new, `charts/rhaii-cluster-validation`)**: `helm install`
   deploys the controller itself as an in-cluster Job, useful for CI/GitOps
   pipelines where there's no interactive kubectl session. It reuses the same
   binary and the same embedded manifests/RBAC-apply code — it does not
   duplicate or reimplement any validation logic.

## Two Job "shapes"

| | Per-node check Jobs | Multi-node Jobs |
|---|---|---|
| Image | validator (`rhaii-validator run`) | tools (iperf3/RDMA) or validator (tcp-lat) |
| GPU request | all GPUs on the node (for nvidia-smi visibility) | 1 per pod |
| Host access | privileged (host sysfs) | none |
| Built via | `deployNodeCheckJobs` (controller) + embedded `node-check-job.yaml` | `jobrunner.BuildJobSpec` |
| Scheduling | one Job per GPU node | `pkg/jobrunner`: ring / star / pairwise |

## Status

This document reflects the target state of the controller-simplification
refactor. See git history / PR description for the incremental steps taken
to get there (each step preserves existing CLI behavior, output format, and
Job semantics — see `CLAUDE.md` "Known Limitations" for things intentionally
left as-is).
