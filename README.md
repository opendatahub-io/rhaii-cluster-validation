# RHAII Cluster Validation

kubectl plugin for validating GPU cluster readiness for AI/ML workloads.

Runs preflight checks on GPU clusters before deploying inference workloads. Validates GPU hardware (drivers, ECC errors, GPU-NIC topology), RDMA connectivity, and cross-node network bandwidth. Auto-detects GPU vendor (NVIDIA/AMD), platform (AKS, EKS, CoreWeave, OpenShift), and cluster topology. Produces a pass/fail report with per-node and per-GPU-NIC results.

**What it checks:**

- GPU driver version and ECC memory errors
- GPU-NIC NUMA topology (which GPU is closest to which NIC)
- RDMA device presence and NIC link status
- TCP bandwidth (iperf3) and latency between node pairs
- RDMA bandwidth (ib_write_bw) per GPU-NIC pair

**Supported platforms:** AKS, EKS, CoreWeave, OpenShift (auto-detected)

## Quick Start

### Prerequisites (Non-OpenShift Clusters)

On OpenShift, pull secrets are managed cluster-wide. On AKS, EKS, or other non-OCP environments, you must pre-create the namespace and configure a pull secret for `registry.redhat.io` before running validation:

```bash
kubectl create namespace rhaii-validation 2>/dev/null || true

kubectl create secret docker-registry rhaii-pull-secret \
  -n rhaii-validation \
  --from-file=.dockerconfigjson=/path/to/your/pull-secret.json \
  2>/dev/null || true
```

Add the pull secret to the serviceaccount
```bash
kubectl create serviceaccount rhaii-validator -n rhaii-validation 2>/dev/null || true
kubectl patch serviceaccount rhaii-validator -n rhaii-validation \
  -p '{"imagePullSecrets": [{"name": "rhaii-pull-secret"}]}'
```
The tool skips SA creation if it already exists, so the patch persists across runs. However, running `kubectl rhaii-validate clean` deletes the SA, so you'll need to re-run the steps above after a clean.

### Option 1: Container Image (No Install)


| Image                                        | Registry                                                            |
| -------------------------------------------- | ------------------------------------------------------------------- |
| Validator                                    | `registry.redhat.io/rhoai/odh-rhaii-cluster-validator-rhel9:v3.4.0` |
| Tools (iperf3, ib_write_bw, ibv_rc_pingpong) | `registry.redhat.io/rhoai/odh-rhaii-validator-tools-rhel9:v3.4.0`   |


Set the image variables and run checks:

```bash
IMG=registry.redhat.io/rhoai/odh-rhaii-cluster-validator-rhel9:v3.4.0
TOOLS=registry.redhat.io/rhoai/odh-rhaii-validator-tools-rhel9:v3.4.0
```

Check required CRDs and operator health:

```bash
podman run --rm -it \
  -v ~/.kube/config:/kubeconfig:z \
  -e KUBECONFIG=/kubeconfig \
  -e RELATED_IMAGE_RHAII_CLUSTER_VALIDATOR=$IMG \
  -e RELATED_IMAGE_RHAII_VALIDATOR_TOOLS=$TOOLS \
  $IMG deps
```

GPU hardware checks (driver version, ECC errors):

```bash
podman run --rm -it \
  -v ~/.kube/config:/kubeconfig:z \
  -e KUBECONFIG=/kubeconfig \
  -e RELATED_IMAGE_RHAII_CLUSTER_VALIDATOR=$IMG \
  -e RELATED_IMAGE_RHAII_VALIDATOR_TOOLS=$TOOLS \
  $IMG gpu
```

TCP bandwidth and latency tests between nodes (requires 2+ GPU nodes):

```bash
podman run --rm -it \
  -v ~/.kube/config:/kubeconfig:z \
  -e KUBECONFIG=/kubeconfig \
  -e RELATED_IMAGE_RHAII_CLUSTER_VALIDATOR=$IMG \
  -e RELATED_IMAGE_RHAII_VALIDATOR_TOOLS=$TOOLS \
  $IMG network
```

RDMA checks (InfiniBand/RoCE clusters only — requires RDMA device plugin and 2+ GPU nodes):

```bash
podman run --rm -it \
  -v ~/.kube/config:/kubeconfig:z \
  -e KUBECONFIG=/kubeconfig \
  -e RELATED_IMAGE_RHAII_CLUSTER_VALIDATOR=$IMG \
  -e RELATED_IMAGE_RHAII_VALIDATOR_TOOLS=$TOOLS \
  $IMG rdma
```

Remove validation resources from the cluster:

```bash
podman run --rm -it \
  -v ~/.kube/config:/kubeconfig:z \
  -e KUBECONFIG=/kubeconfig \
  $IMG clean
```

> **Note:** `clean` deletes the ServiceAccount and namespace. If you configured a pull secret in the [prerequisites](#prerequisites-non-openshift-clusters), you'll need to re-run those steps before the next validation run.

> **Upstream images:** For development or non-RHOAI use, substitute `quay.io/opendatahub/odh-rhaii-cluster-validator:latest` and `quay.io/opendatahub/odh-rhaii-validator-tools:odh-stable`.

#### Running All Checks Together

You can run all checks (deps + gpu + network + rdma) in a single command:

```bash
podman run --rm -it \
  -v ~/.kube/config:/kubeconfig:z \
  -e KUBECONFIG=/kubeconfig \
  -e RELATED_IMAGE_RHAII_CLUSTER_VALIDATOR=$IMG \
  -e RELATED_IMAGE_RHAII_VALIDATOR_TOOLS=$TOOLS \
  $IMG all
```

Note that `all` includes RDMA checks and may fail or time out on environments without RDMA support (Infiniband or RoCE). Use the individual subcommands above if your environment does not support RDMA.

### Option 2: Download Binary (Linux, No Build Required)

Extract the binary from the container image and install it locally as a kubectl plugin:

```bash
IMG=registry.redhat.io/rhoai/odh-rhaii-cluster-validator-rhel9:v3.4.0

podman create --name rhaii-extract $IMG
podman cp rhaii-extract:/usr/local/bin/rhaii-validator kubectl-rhaii_validate
podman rm rhaii-extract
chmod +x kubectl-rhaii_validate
sudo mv kubectl-rhaii_validate /usr/local/bin/
```

Or using `make` (override `IMG` to use the production image):

```bash
make download IMG=registry.redhat.io/rhoai/odh-rhaii-cluster-validator-rhel9:v3.4.0
```

Once installed, you can run checks using kubectl:
```bash
# NOTE: the binary defaults to upstream images baked in at build time. Override with environment variables to use  production images
export RELATED_IMAGE_RHAII_CLUSTER_VALIDATOR=registry.redhat.io/rhoai/odh-rhaii-cluster-validator-rhel9:v3.4.0
export RELATED_IMAGE_RHAII_VALIDATOR_TOOLS=registry.redhat.io/rhoai/odh-rhaii-validator-tools-rhel9:v3.4.0

kubectl rhaii-validate gpu              # GPU hardware checks
kubectl rhaii-validate network          # TCP bandwidth + latency tests
kubectl rhaii-validate rdma             # All RDMA checks + connectivity + bandwidth
kubectl rhaii-validate all              # Everything (deps + gpu + network + rdma)
kubectl rhaii-validate all --debug      # Keep pods alive for inspection
kubectl rhaii-validate all -o json      # JSON output
kubectl rhaii-validate clean            # Cleanup
```

> **macOS users:** The container image contains a Linux binary. Use `make install` to [build from source](#building-from-source) instead.

## How Each Check Works

### GPU Driver Version

Runs `nvidia-smi` (or `rocm-smi` for AMD) on the host via `chroot /host`:

```
chroot /host nvidia-smi --query-gpu=driver_version,name,memory.total --format=csv,noheader,nounits
```

Output:

```
580.126.09, NVIDIA A100 80GB PCIe, 81920
580.126.09, NVIDIA A100 80GB PCIe, 81920
```

Parses driver version and compares against `min_driver_version` from platform config.

PASS: `NVIDIA driver: 580.126.09, GPU: NVIDIA A100 80GB PCIe (81920 MiB), 2 GPU(s)`
FAIL: `Driver version 530.0 is below minimum 535.0`

### GPU ECC Errors

Queries uncorrectable (hardware) ECC errors per GPU:

```
chroot /host nvidia-smi --query-gpu=index,ecc.errors.uncorrected.volatile.total --format=csv,noheader,nounits
```

Output:

```
0, 0
1, 0
```

Each row = one GPU. If error count > 0, that GPU has memory corruption.

PASS: `No uncorrectable ECC errors on 2 GPU(s)`
FAIL: `Uncorrectable ECC errors found: GPU 1: 3 uncorrectable errors`
Remediation: Replace GPU or contact cloud provider

### GPU-NIC Topology

Discovers which RDMA NIC is closest to which GPU based on NUMA affinity:

```
# GPU NUMA node
chroot /host cat /sys/class/nvidia/nvidia0/numa_node
# → 0

# NIC NUMA node
chroot /host cat /sys/class/infiniband/mlx5_0/device/numa_node
# → 0

# Same NUMA = optimal path (GPU0 ↔ mlx5_0)
```

On bare metal (8 GPUs, 8 NICs):

```text
GPU0↔mlx5_0(NUMA0), GPU1↔mlx5_1(NUMA0), GPU2↔mlx5_2(NUMA0), GPU3↔mlx5_3(NUMA0)
GPU4↔mlx5_4(NUMA1), GPU5↔mlx5_5(NUMA1), GPU6↔mlx5_6(NUMA1), GPU7↔mlx5_7(NUMA1)
```

On VMs (NUMA hidden):

```text
GPU0↔mlx5_0(NUMA-1), GPU1↔mlx5_0(NUMA-1)
```

The controller uses this mapping to set `-d mlx5_X --use_cuda Y` on RDMA bandwidth jobs.

### RDMA Devices

Checks RDMA device presence. Tries `ibv_devices` on host, falls back to sysfs:

```
# Primary: ibv_devices
chroot /host ibv_devices

# Fallback: sysfs
chroot /host ls /sys/class/infiniband/
# → mlx5_0
```

PASS: `1 RDMA device(s) found: mlx5_0`
FAIL: `No RDMA devices found`

### RDMA NIC Status

Checks RDMA NIC link state and speed. Tries `ibstat` on host, falls back to sysfs:

```
# Primary: ibstat (shows Active/Down, link speed)
chroot /host ibstat

# Fallback: sysfs device listing
chroot /host ls /sys/class/infiniband/
```

PASS: `2 active RDMA NIC(s): mlx5_0 (200 Gbps), mlx5_1 (200 Gbps)`
WARN: `ibstat not available on host, 1 RDMA device(s) found via sysfs: mlx5_0`
FAIL: `RDMA NIC(s) down: mlx5_1`

Why WARN on AKS: `ibstat` (from `infiniband-diags` package) is not installed on AKS VM hosts.
The check falls back to sysfs which confirms the device exists but cannot verify link state or speed.
This is expected on AKS — not a problem. On bare metal (OCP/CoreWeave) where `ibstat` is installed, it shows PASS with full details.

### TCP Bandwidth (iperf3)

Runs iperf3 server/client jobs between node pairs using ring topology:

```
# Server pod on node-1
iperf3 -s

# Client pod on node-2
iperf3 -c <server-pod-ip> -t 10 -J
```

Parses JSON output for `end.sum_sent.bits_per_second`, converts to Gbps.
Compares against `thresholds.tcp_bandwidth_gbps` from platform config.

PASS: `TCP bandwidth: 94.5 Gbps (threshold: 25 Gbps)`
WARN: `TCP bandwidth: 15.0 Gbps (below 25 Gbps threshold)`
FAIL: `TCP bandwidth: 7.6 Gbps (well below 25 Gbps threshold)`

Image: `quay.io/opendatahub/odh-rhaii-validator-tools:odh-stable` (default: `quay.io/opendatahub/odh-rhaii-validator-tools:odh-stable`)

### RDMA Bandwidth (ib_write_bw)

Runs RDMA write bandwidth test per GPU-NIC pair. Uses topology to set the right device and GPU:

```
# Server pod on node-1
ib_write_bw --duration 10 -d mlx5_0 --use_cuda 0

# Client pod on node-2
ib_write_bw --duration 10 -d mlx5_0 --use_cuda 0 <server-pod-ip>
```

On a node with 8 GPUs and 8 NICs, this creates 8 separate jobs:

```
ib-write-bw-mlx5-0: -d mlx5_0 --use_cuda 0
ib-write-bw-mlx5-1: -d mlx5_1 --use_cuda 1
...
ib-write-bw-mlx5-7: -d mlx5_7 --use_cuda 7
```

Parses `BW average [MB/sec]` from output, converts to Gbps.
Compares against `thresholds.rdma_bandwidth_pd_gbps` from platform config.

PASS: `RDMA bandwidth: 195.2 Gbps (threshold: 180 Gbps)`
FAIL: `RDMA bandwidth: 50.1 Gbps (well below 180 Gbps threshold)`

Image: `quay.io/opendatahub/odh-rhaii-validator-tools:odh-stable` (default: `quay.io/opendatahub/odh-rhaii-validator-tools:odh-stable`)

## Ring Topology

By default, bandwidth tests use ring topology so every node is tested as both sender and receiver:

```
8 nodes:
  Round 1: node-2 → node-1 (iperf3 + RDMA per NIC)
  Round 2: node-3 → node-2
  Round 3: node-4 → node-3
  ...
  Round 8: node-1 → node-8
```

Override with star topology:

```bash
kubectl rhaii-validate rdma --server-node node-1
```

## Report

```
=== Validation Report ===
Platform: AKS

GPU-NIC Topology:
  gpu-node-0: GPU0↔mlx5_0(NUMA0), GPU1↔mlx5_1(NUMA0)
  gpu-node-1: GPU0↔mlx5_0(NUMA0), GPU1↔mlx5_1(NUMA0)

GROUP                CHECK                NODE            STATUS   MESSAGE
---------------------------------------------------------------------------
gpu_hardware         gpu_driver_version   gpu-node-0      PASS     NVIDIA driver: 580.126.09
gpu_hardware         gpu_ecc_status       gpu-node-0      PASS     No uncorrectable ECC errors
topology             gpu_nic_topology     gpu-node-0      PASS     2 GPU(s), 1 NIC(s)
networking_rdma      rdma_devices_detected gpu-node-0     PASS     1 RDMA device(s) found
bandwidth            iperf3-tcp           node1 → node0   PASS     TCP: 94.5 Gbps
bandwidth            ib-write-bw-mlx5-0   node1 → node0   PASS     RDMA GPU0: 195.2 Gbps

Summary: 6 PASS | 0 WARN | 0 FAIL | 0 SKIP
Status:  READY

Report:
  kubectl get cm rhaii-validate-report -n rhaii-validation -o jsonpath='{.data.report\.json}' | jq .
```

## Cluster Prerequisites

### Required


| Requirement                                           | Why                                         | Verified Platforms  |
| ----------------------------------------------------- | ------------------------------------------- | ------------------- |
| GPU nodes with NVIDIA or AMD GPUs                     | GPU driver, ECC, topology checks            | AKS, OCP, CoreWeave |
| GPU driver installed on nodes                         | `nvidia-smi` / `rocm-smi` must work on host | All                 |
| `nvidia.com/gpu` or `amd.com/gpu` in node allocatable | Auto-discovers GPU nodes                    | All                 |
| Cluster-admin or namespace-admin RBAC                 | Creates namespace, RBAC, Jobs               | All                 |


### Required for Networking Tests


| Requirement                   | Why                                                                                                                                   |
| ----------------------------- | ------------------------------------------------------------------------------------------------------------------------------------- |
| 2+ GPU nodes                  | Ring topology needs at least 2 nodes                                                                                                  |
| Job container images pullable | Override with `--image`, `--tools-image`, or env vars `RELATED_IMAGE_RHAII_CLUSTER_VALIDATOR` / `RELATED_IMAGE_RHAII_VALIDATOR_TOOLS` |


### Required for RDMA Tests


| Requirement                              | Why                                                              |
| ---------------------------------------- | ---------------------------------------------------------------- |
| RDMA device plugin running               | Exposes RDMA resources to pods                                   |
| RDMA resource in `jobs.resources` config | e.g., `nvidia.com/roce: "1"` or `rdma/rdma_shared_device_a: "1"` |
| InfiniBand/RoCE NICs on nodes            | `mlx5_*` devices in `/sys/class/infiniband/`                     |


### Platform-Specific Notes

**AKS:**

- Use ND-series VMs for RDMA (NC-series has GPUs but no InfiniBand)
- GPU label `nvidia.com/gpu.present=true` auto-detected by GPU operator
- `ibstat`/`ibv_devices` may not be on host — sysfs fallback used

**OpenShift (OCP):**

- Privileged SCC auto-granted to `rhaii-validator` service account
- GPU operator must be installed (`nvidia-gpu-operator` namespace)
- For RDMA: Network Operator with RDMA shared device plugin
- Add RDMA resource to config: `oc edit cm rhaii-validate-config -n rhaii-validation`

**CoreWeave:**

- GPU nodes may not have `nvidia.com/gpu.present` label — auto-discovered from node allocatable resources
- Per-node Jobs use hostname affinity instead of label selector
- RDMA device plugin in `cw-rdma` namespace

**EKS:**

- GPU label auto-detected
- EFA (Elastic Fabric Adapter) instead of InfiniBand for RDMA
- EFA device plugin exposes `efa` resources

### What Gets Auto-Detected (No Config Needed)


| Setting                          | How                                                          |
| -------------------------------- | ------------------------------------------------------------ |
| GPU vendor (NVIDIA/AMD)          | Node labels or allocatable resources                         |
| GPU node discovery               | `nvidia.com/gpu.present` label, fallback to allocatable scan |
| Platform (AKS/OCP/EKS/CoreWeave) | Node labels and provider ID                                  |
| GPU count per node               | `node.status.allocatable`                                    |
| GPU-NIC topology                 | sysfs NUMA affinity                                          |
| OpenShift SCC                    | Auto-created when OCP detected                               |


### What You Configure (Platform YAML or ConfigMap)


| Setting              | Where                               | Example                       |
| -------------------- | ----------------------------------- | ----------------------------- |
| Min driver version   | `gpu.min_driver_version`            | `"535.0"`                     |
| Pod resources        | `agent.resources`, `jobs.resources` | `cpu: "500m"`                 |
| RDMA resource        | `jobs.resources`                    | `nvidia.com/roce: "1"`        |
| Bandwidth thresholds | `thresholds.`*                      | `pass: 25, warn: 10, fail: 5` |


## Architecture

- GPU vendor (NVIDIA/AMD) auto-detected from node labels or allocatable resources
- GPU tools run on host via `chroot /host` (privileged per-node Jobs)
- Bandwidth jobs use ring topology (every node tested as sender + receiver)
- RDMA tests expanded per GPU-NIC pair using discovered topology
- RDMA tests skipped if no RDMA resource configured
- Report stored in ConfigMap for persistence
- Default images defined in `manifests/image-references/image-references.yaml` (embedded via `//go:embed`), overridable with env vars (`RELATED_IMAGE_RHAII_CLUSTER_VALIDATOR`, `RELATED_IMAGE_RHAII_VALIDATOR_TOOLS`) or CLI flags (`--image`, `--tools-image`)

## GPU Vendor Support


| Vendor | Driver Check | ECC Check              | Bandwidth Jobs               |
| ------ | ------------ | ---------------------- | ---------------------------- |
| NVIDIA | nvidia-smi   | nvidia-smi (ECC query) | iperf3, ib_write_bw          |
| AMD    | rocm-smi     | rocm-smi (RAS query)   | Skipped (NVIDIA-only images) |


Vendor is auto-detected. No configuration needed.

## Building from Source

```bash
make install
kubectl rhaii-validate all
```

## Development

```bash
make build              # Build binary
make test               # Run unit tests
make lint               # Run linter
make install            # Build + install as kubectl plugin
make container          # Build validator container image
make container-rdma     # Build tools container image
make run-local          # Run checks locally (requires GPU node)
```

Container images are automatically built and pushed to `quay.io/opendatahub` via Tekton on merge to `main`.

See [docs/platform-config.md](docs/platform-config.md) for per-platform configuration examples (OCP, AKS, CoreWeave, EKS).
See [CLAUDE.md](CLAUDE.md) for full developer docs.
See [docs/dev.md](docs/dev.md) for odh-cli integration guide.