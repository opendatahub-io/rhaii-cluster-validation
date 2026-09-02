# Platform Configuration Guide

This guide shows how to configure `rhaii-validator` for different cluster setups.

## Config Location

Platform config is stored in a ConfigMap. It's auto-created on first run with detected defaults.

```bash
# View current config
kubectl get cm rhaii-validate-config -n rhaii-validation -o yaml

# Edit config
kubectl edit cm rhaii-validate-config -n rhaii-validation

# Config is preserved across runs — edit once, reuse on every run
```

## Config Structure

```yaml
platform: OCP                        # auto-detected

agent:                               # Per-node check Job pods (GPU/RDMA checks)
  requests:
    cpu: "500m"
    memory: "512Mi"
  limits: {}                         # no cpu/memory limits
  annotations: {}

jobs:                                # Bandwidth test pods (iperf3, ib_write_bw)
  requests:
    cpu: "500m"
    memory: "512Mi"
    # nvidia.com/roce: "8"           # RDMA resource (manual, see below)
  limits:
    # nvidia.com/roce: "8"           # must match requests for device resources
  annotations: {}

gpu:
  min_driver_version: "535.0"        # minimum NVIDIA/AMD driver version

thresholds:
  tcp_bandwidth_gbps:
    pass: 25
    warn: 10
    fail: 5
  rdma_bandwidth_pd_gbps:
    pass: 180
    warn: 100
    fail: 50
```

## What's Auto-Detected (Don't Configure)

| Setting | How |
|---------|-----|
| Platform (AKS/OCP/EKS/CoreWeave) | Node labels, provider ID |
| GPU vendor (NVIDIA/AMD) | Node labels or allocatable resources |
| GPU nodes | `nvidia.com/gpu.present` label, fallback to allocatable scan |
| GPU count per node | `node.status.allocatable` |
| GPU resource for jobs | `nvidia.com/gpu` or `amd.com/gpu` (added to requests+limits) |
| GPU-NIC topology | sysfs NUMA affinity |
| OpenShift SCC | Auto-created when OCP detected |

## What You Configure

| Setting | Where | When |
|---------|-------|------|
| CPU/memory for pods | `agent.requests`, `jobs.requests` | Always (has defaults) |
| RDMA resource | `jobs.requests` + `jobs.limits` | Only if RDMA tests needed |
| Min driver version | `gpu.min_driver_version` | If your cluster needs a different minimum |
| Bandwidth thresholds | `thresholds.*` | If defaults don't match your hardware |
| Pod annotations | `agent.annotations`, `jobs.annotations` | If pods need special annotations |

## Platform-Specific Examples

### OpenShift (OCP) with NVIDIA Network Operator (RoCE)

```bash
# Check RDMA resource name
oc get nodes -o json | jq '.items[] | {name: .metadata.name, rdma: (.status.allocatable | to_entries[] | select(.key | test("rdma|roce|hca")))}'

# Check RDMA device plugin
oc get pods -A | grep -i rdma
```

```yaml
platform: OCP

agent:
  requests:
    cpu: "500m"
    memory: "512Mi"
  annotations: {}

jobs:
  requests:
    cpu: "500m"
    memory: "512Mi"
    nvidia.com/roce: "1"
  limits:
    nvidia.com/roce: "1"
  annotations: {}

gpu:
  min_driver_version: "535.0"

thresholds:
  tcp_bandwidth_gbps:
    pass: 25
    warn: 10
    fail: 5
  rdma_bandwidth_pd_gbps:
    pass: 180
    warn: 100
    fail: 50
```

### OpenShift with RDMA Shared Device Plugin

```yaml
jobs:
  requests:
    cpu: "500m"
    memory: "512Mi"
    rdma/rdma_shared_device_a: "1"
  limits:
    rdma/rdma_shared_device_a: "1"
```

### AKS with InfiniBand (ND-series VMs)

```bash
# Check if RDMA resources exist
kubectl get nodes -o json | jq '.items[] | {name: .metadata.name, rdma: (.status.allocatable | to_entries[] | select(.key | test("rdma|ib|hca")))}'
```

```yaml
platform: AKS

jobs:
  requests:
    cpu: "500m"
    memory: "512Mi"
    rdma/hca: "1"
  limits:
    rdma/hca: "1"

gpu:
  min_driver_version: "535.0"
```

Note: AKS NC-series VMs have GPUs but no InfiniBand. Use ND-series for RDMA.

### AKS without RDMA (NC-series VMs)

```yaml
platform: AKS

jobs:
  requests:
    cpu: "500m"
    memory: "512Mi"
  limits: {}
  # No RDMA resource — RDMA tests will be skipped

gpu:
  min_driver_version: "535.0"
```

### CoreWeave

```bash
# Check RDMA resource name
kubectl get nodes -o json | jq '.items[] | {name: .metadata.name, rdma: (.status.allocatable | to_entries[] | select(.key | test("rdma|roce|ib")))}'
```

```yaml
platform: CoreWeave

jobs:
  requests:
    cpu: "500m"
    memory: "512Mi"
    # Add your cluster's RDMA resource here
  limits: {}

gpu:
  min_driver_version: "535.0"
```

### EKS with EFA (Elastic Fabric Adapter)

```yaml
platform: EKS

jobs:
  requests:
    cpu: "500m"
    memory: "512Mi"
    vpc.amazonaws.com/efa: "1"
  limits:
    vpc.amazonaws.com/efa: "1"

gpu:
  min_driver_version: "535.0"
```

> **Note on EFA/SRD connectivity testing (pingmesh):** the RDMA connectivity
> mesh (`rdma-ping`) determines how many EFA NICs to request for a node pair
> by reading only the *server* node's allocatable EFA count. This assumes
> both nodes in a pair are the same EC2 instance type and therefore have the
> same EFA NIC count — true within a single RDMA/SRD network domain (e.g. all
> `p5` nodes), but not guaranteed across domains (e.g. `p5` and `p6` nodes
> aren't expected to be in the same SRD domain and wouldn't normally be
> paired for pingmesh anyway). If a node pair somehow mixes instance types
> with different EFA counts, only the server's count is requested for both
> pods.

## How to Find Your RDMA Resource Name

```bash
# Step 1: Check if RDMA device plugin is running
kubectl get pods -A | grep -i rdma

# Step 2: Check what resources it exposes
kubectl get nodes -o json | jq '.items[] | {
  name: .metadata.name,
  resources: (.status.allocatable | to_entries[] | select(.key | test("rdma|roce|hca|efa|ib")))
}'

# Step 3: Add to config
kubectl edit cm rhaii-validate-config -n rhaii-validation
# Add the resource name under jobs.requests AND jobs.limits
```

Common RDMA resource names:

| Resource | Provider |
|----------|----------|
| `nvidia.com/roce` | NVIDIA Network Operator (RoCE) |
| `rdma/rdma_shared_device_a` | RDMA shared device plugin |
| `rdma/hca` | Some AKS/bare metal setups |
| `vpc.amazonaws.com/efa` | AWS EFA device plugin |

## Adjusting Thresholds

Default thresholds are for high-end GPU clusters (H100, A100). Adjust for your hardware:

```yaml
thresholds:
  tcp_bandwidth_gbps:
    pass: 25          # >= 25 Gbps = PASS
    warn: 10          # >= 10 Gbps = WARN
    fail: 5           # < 5 Gbps = FAIL (below warn*0.4 threshold)
  rdma_bandwidth_pd_gbps:
    pass: 180         # >= 180 Gbps = PASS (single NIC, H100)
    warn: 100
    fail: 50
```

For AKS VMs (lower bandwidth expected):

```yaml
thresholds:
  tcp_bandwidth_gbps:
    pass: 5
    warn: 2
    fail: 1
```

## Adding Pod Annotations

Some clusters require annotations for networking (e.g., Multus, Istio):

```yaml
jobs:
  annotations:
    k8s.v1.cni.cncf.io/networks: rdma-net
    sidecar.istio.io/inject: "false"
```

## Applying Config Before First Run

```bash
# Create namespace
kubectl create namespace rhaii-validation

# Apply custom config
cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: ConfigMap
metadata:
  name: rhaii-validate-config
  namespace: rhaii-validation
data:
  platform.yaml: |
    jobs:
      requests:
        cpu: "500m"
        memory: "512Mi"
        nvidia.com/roce: "1"
      limits:
        nvidia.com/roce: "1"
    gpu:
      min_driver_version: "535.0"
    thresholds:
      tcp_bandwidth_gbps:
        pass: 25
        warn: 10
        fail: 5
EOF

# Run validation — uses your pre-created config
kubectl rhaii-validate all
```
