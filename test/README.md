# Testing the RHAII Cluster Validation Agent

## 1. Local Testing (No Cluster)

Run the agent binary locally. GPU/RDMA checks will fail gracefully since there's no GPU hardware:

```bash
make build
make run-local
```

Expected output: JSON report with FAIL/SKIP for GPU and RDMA checks.

## 2. Unit Tests

```bash
make test
```

## 3. Container Build and Test

### Build the image

```bash
# Default image name
make container

# Custom image (e.g., your personal Quay)
make container IMG=quay.io/{user}/rhaii-validate-agent:dev
```

### Run the container locally

```bash
podman run --rm quay.io/{user}/rhaii-validate-agent:dev run --node-name local-test
```

### Verify tools are installed in the container

```bash
podman run --rm --entrypoint /bin/bash quay.io/{user}/rhaii-validate-agent:dev -c "
  echo '=== Installed Tools ==='
  which ibv_devices && echo 'ibv_devices: OK' || echo 'ibv_devices: MISSING'
  which ibstat && echo 'ibstat: OK' || echo 'ibstat: MISSING'
  which iperf3 && echo 'iperf3: OK' || echo 'iperf3: MISSING'
  which ib_write_bw && echo 'ib_write_bw: OK' || echo 'ib_write_bw: MISSING'
  echo ''
  echo '=== RPM Packages ==='
  rpm -qa | grep -E 'rdma|iperf|perftest|libibverbs' | sort
"
```

## 4. Cluster Testing (GPU Nodes Required)

### Prerequisites

- Kubernetes cluster with GPU nodes (`nvidia.com/gpu.present=true` label)
- `kubectl` configured and authenticated
- Image pushed to a registry accessible from the cluster

### Push the image

```bash
make push IMG=quay.io/{user}/rhaii-validate-agent:dev
```

### Deploy and collect results

```bash
# Deploy RBAC + DaemonSet
make run IMG=quay.io/{user}/rhaii-validate-agent:dev

# Check pod status
kubectl get pods -n rhaii-validation -o wide

# Collect JSON results from all agent pods
make logs

# Cleanup
make clean
```

### Test on a single GPU node (without DaemonSet)

```bash
GPU_NODE=$(kubectl get nodes -l nvidia.com/gpu.present=true -o jsonpath='{.items[0].metadata.name}')

kubectl run rhaii-validate-test \
  --rm -it \
  --image=quay.io/{user}/rhaii-validate-agent:dev \
  --overrides='{
    "spec": {
      "nodeName": "'$GPU_NODE'",
      "containers": [{
        "name": "rhaii-validate-test",
        "image": "quay.io/{user}/rhaii-validate-agent:dev",
        "args": ["run", "--node-name", "'$GPU_NODE'"],
        "securityContext": {"privileged": true},
        "resources": {"limits": {"nvidia.com/gpu": "1"}}
      }],
      "restartPolicy": "Never"
    }
  }'
```

### Test with bandwidth checks

```bash
# Start iperf3 server on one node
kubectl run iperf-server \
  --image=registry.access.redhat.com/ubi9/ubi-minimal \
  --command -- sleep 3600

kubectl exec iperf-server -- microdnf install -y iperf3
kubectl exec iperf-server -- iperf3 -s -D

IPERF_IP=$(kubectl get pod iperf-server -o jsonpath='{.status.podIP}')

# Run agent with bandwidth test
kubectl run rhaii-validate-bw \
  --rm -it \
  --image=quay.io/{user}/rhaii-validate-agent:dev \
  --overrides='{
    "spec": {
      "nodeName": "'$GPU_NODE'",
      "containers": [{
        "name": "rhaii-validate-bw",
        "image": "quay.io/{user}/rhaii-validate-agent:dev",
        "args": ["run", "--node-name", "'$GPU_NODE'", "--bandwidth", "--iperf-server", "'$IPERF_IP'"],
        "securityContext": {"privileged": true},
        "resources": {"limits": {"nvidia.com/gpu": "1"}}
      }],
      "restartPolicy": "Never"
    }
  }'

# Cleanup iperf server
kubectl delete pod iperf-server
```

## 5. Validating JSON Output

Pipe the agent output through `jq` to verify the JSON structure:

```bash
# From local run
./bin/rhaii-validate-agent --node-name test | jq .

# From pod logs
kubectl logs -n rhaii-validation <pod-name> | jq .

# Check specific results
kubectl logs -n rhaii-validation <pod-name> | jq '.results[] | select(.status == "FAIL")'

# Count by status
kubectl logs -n rhaii-validation <pod-name> | jq '[.results[].status] | group_by(.) | map({(.[0]): length}) | add'
```

## 6. Expected Results

### On a GPU node with RDMA

```json
{
  "node": "aks-gpuh100-vmss000000",
  "results": [
    {"category": "gpu_hardware", "name": "gpu_driver_version", "status": "PASS"},
    {"category": "gpu_hardware", "name": "gpu_ecc_status", "status": "PASS"},
    {"category": "networking_rdma", "name": "rdma_devices_detected", "status": "PASS"},
    {"category": "networking_rdma", "name": "rdma_nic_status", "status": "PASS"}
  ]
}
```

### On a node without GPU

```json
{
  "node": "cpu-node",
  "results": [
    {"category": "gpu_hardware", "name": "gpu_driver_version", "status": "FAIL"},
    {"category": "gpu_hardware", "name": "gpu_ecc_status", "status": "SKIP"},
    {"category": "networking_rdma", "name": "rdma_devices_detected", "status": "FAIL"},
    {"category": "networking_rdma", "name": "rdma_nic_status", "status": "FAIL"}
  ]
}
```
