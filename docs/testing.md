# Testing rhaii-cluster-validation Locally

How to build the validator from source, push images to a registry, and run it
against a live cluster. Use this to verify local changes (e.g. the
`pkg/controller` refactor) end-to-end.

## Prerequisites

- `kubectl` pointed at the target cluster (`kubectl config current-context`)
- `go` 1.26+ (use `GOTOOLCHAIN=auto` if you hit a version mismatch)
- `podman` (or `docker`) for building/pushing images
- A container registry you can push to (e.g. `quay.io/<user>`)
- Cluster access with permission to create namespaces, ClusterRoles, and Jobs

### Cluster requirements per check

| Check            | Needs GPU nodes | Needs RDMA NICs | Notes                                  |
|------------------|-----------------|-----------------|----------------------------------------|
| `deps`           | no              | no              | API-only (CRDs + operator health)      |
| `network`        | no*             | no              | TCP iperf3 + latency; *runs on GPU nodes if present, else discovered nodes |
| `gpu`            | yes             | no              | driver/ECC via nvidia-smi              |
| `rdma-node`      | yes             | yes             | topology, devices, NIC status          |
| `rdma-ping`      | yes             | yes             | needs prior `rdma-node` run            |
| `rdma-bandwidth` | yes             | yes             | needs prior `rdma-node` run            |
| `all`            | yes             | yes             | deps + gpu + network + rdma            |

> On a CPU-only cluster, `gpu` and `rdma` find no GPU nodes and effectively
> SKIP. Use `deps` and `network` for a smoke test there; switch to a GPU
> cluster for a full run.

### Confirm the cluster has GPU / RDMA nodes

```bash
# GPU node labels
kubectl get nodes -o json | jq -r '.items[] |
  "\(.metadata.name): nvidia=\(.metadata.labels["nvidia.com/gpu.present"] // "none") amd=\(.metadata.labels["amd.com/gpu.present"] // "none")"'

# GPU / RDMA allocatable resources
kubectl get nodes -o json | jq -r '.items[] |
  "\(.metadata.name): \(.status.allocatable | to_entries | map(select(.key|test("nvidia|amd|rdma|roce"))) | from_entries)"'
```

## Option A: Local binary only (fastest)

Tests the **controller logic** (which runs on your machine) against the
cluster, while per-node Job pods use the images already baked into the binary
via `manifests/image-references/image-references.yaml`. Good for iterating on
`pkg/controller`, `pkg/config`, report formatting, scheduling, etc.

```bash
GOTOOLCHAIN=auto make build      # compile bin/rhaii-validator (version = git HEAD short SHA)
make install                     # install as kubectl plugin at /usr/local/bin/kubectl-rhaii_validate

kubectl rhaii-validate deps      # quick API-only smoke test
kubectl rhaii-validate network   # TCP bandwidth + latency
```

This does **not** exercise the validator/tools container images with your
local changes — only the controller half. For that, use Option B.

## Option B: Build + push images (full end-to-end)

Tests both halves: your controller code **and** the per-node/multi-node Job
pods running your freshly built images inside the cluster.

Set your registry namespace once:

```bash
export QUAY_USER=<your-quay-user>          # e.g. aputtur
export IMG=quay.io/$QUAY_USER/odh-rhaii-cluster-validator:latest
export IMG_TOOLS=quay.io/$QUAY_USER/odh-rhaii-validator-tools:latest
```

Build and push both images:

```bash
make container      IMG=$IMG            # validator image (Dockerfile.dev)
make container-rdma IMG_TOOLS=$IMG_TOOLS # tools image: iperf3, ib_write_bw, ibv_rc_pingpong (tools/Dockerfile.dev)
make push           IMG=$IMG
make push-rdma      IMG_TOOLS=$IMG_TOOLS
```

> New quay.io repos are **private** by default. Make both repos public
> (quay.io repo → Settings → Make Public), or supply a pull secret — see
> "Pull secrets" below.
>
> If `podman` caches stale layers, add `--no-cache` to the container build, or
> bump the tag.

Install the plugin and point it at your images, then run:

```bash
make install
export RELATED_IMAGE_RHAII_CLUSTER_VALIDATOR=$IMG
export RELATED_IMAGE_RHAII_VALIDATOR_TOOLS=$IMG_TOOLS

kubectl rhaii-validate all
```

The `RELATED_IMAGE_*` env vars override the embedded defaults. You can also
pass `--image` / `--tools-image` flags instead of the env vars.

## Running checks

```bash
kubectl rhaii-validate <check> [flags]
```

Checks: `deps`, `network`, `gpu`, `rdma-node`, `rdma-ping`, `rdma-bandwidth`,
`rdma`, `all`.

Common flags:

- `--debug` — keep Job pods alive after the run for `kubectl exec` / `kubectl logs`
- `-o json` — JSON output instead of the table
- `--nodes <n1,n2>` — restrict to specific GPU nodes
- `--server-node <n>` / `--client-nodes <n1,n2>` — pin topology for network/rdma/bandwidth
- `--namespace <ns>` — override the default `rhaii-validation` namespace
- `--pull-secret <name>` — attach an image-pull secret to the workload ServiceAccount

### Suggested progression when testing the refactor

```bash
kubectl rhaii-validate deps            # API-only, no pods — quickest
kubectl rhaii-validate gpu             # per-node GPU check jobs
kubectl rhaii-validate rdma-node       # topology/devices; produces topology used by ping/bandwidth
kubectl rhaii-validate rdma-ping       # connectivity mesh (needs rdma-node first)
kubectl rhaii-validate rdma-bandwidth  # ib_write_bw per GPU-NIC pair (needs rdma-node first)
kubectl rhaii-validate all             # full end-to-end
```

## Inspecting results

The JSON report is stored in a ConfigMap and merged across runs:

```bash
kubectl get cm rhaii-validate-report -n rhaii-validation \
  -o jsonpath='{.data.report\.json}' | jq .
```

Pingmesh failures (when present) go to a separate ConfigMap:

```bash
kubectl get cm rhaii-validate-pingmesh-failures -n rhaii-validation \
  -o jsonpath='{.data.failures\.json}' | jq .
```

Inspect live/failed pods (use `--debug` so they aren't cleaned up):

```bash
kubectl get pods -n rhaii-validation
kubectl logs -n rhaii-validation <pod>
kubectl exec -it -n rhaii-validation <pod> -- bash
```

## Pull secrets (private registries / registry.redhat.io)

If your images are private, or you use `registry.redhat.io` on a non-OCP
cluster, create a docker-registry secret in the validation namespace and pass
it:

```bash
kubectl create namespace rhaii-validation
kubectl create secret docker-registry my-pull-secret \
  --docker-server=quay.io \
  --docker-username=<user> \
  --docker-password=<token> \
  -n rhaii-validation

kubectl rhaii-validate all --pull-secret my-pull-secret
```

## Cleanup

```bash
kubectl rhaii-validate clean        # remove validation Jobs, RBAC, and pingmesh-failures CM
```

`clean` deletes the check Jobs, the RBAC resources (ServiceAccount,
ClusterRole, ClusterRoleBinding, and the OpenShift SCC binding), and the
`rhaii-validate-pingmesh-failures` ConfigMap. It **preserves** the
`rhaii-validate-report` and `rhaii-validate-config` ConfigMaps and the
namespace, so a later run can reuse config and merge into the existing report.
Delete those manually if you want a fully clean slate:

```bash
kubectl delete cm rhaii-validate-report rhaii-validate-config -n rhaii-validation
```

## Troubleshooting

- **Go version mismatch on build** — prefix with `GOTOOLCHAIN=auto`.
- **`gpu`/`rdma` report no nodes** — the cluster has no GPU nodes; confirm with
  the node-label query above, or switch kube-context.
- **`ImagePullBackOff` on Job pods** — image is private (make it public) or the
  pull secret is missing/wrong; check `kubectl describe pod -n rhaii-validation <pod>`.
- **`ibv_devices`/`ibstat` missing on AKS** — expected; the checks fall back to
  reading sysfs via privileged mode.
- **Stale image after rebuild** — bump the tag or rebuild with `--no-cache`;
  `IfNotPresent` pull policy can serve a cached node image otherwise.
- **OpenShift** — the controller also creates an SCC binding for the workload
  ServiceAccount (needed for host sysfs visibility); requires your user to have
  the RBAC `bind` verb on `system:openshift:scc:privileged`.

## Notes

- Controller mode runs on your machine; agent mode (`run`, hidden) runs inside
  Job pods. The controller parses JSON from pod logs, skipping any leading
  non-JSON progress lines.
- `rdma-ping` and `rdma-bandwidth` depend on topology produced by a prior
  `rdma-node` run in the same namespace.
- The report ConfigMap is merged across runs, so partial runs (e.g. just
  `rdma-ping`) preserve sections produced by earlier runs.
