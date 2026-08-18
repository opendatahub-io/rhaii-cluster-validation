# rhaii-cluster-validation Helm chart

Runs the `rhaii-validator` controller as an in-cluster Job. This is for
CI/GitOps pipelines that have no interactive `kubectl` session — it does **not**
replace `kubectl rhaii-validate`, which remains the primary way to run
validation from an operator's machine.

The behavior is identical to running `kubectl rhaii-validate <checkMode>`: the
chart runs the same controller binary with the same flags, only in-cluster
instead of on your laptop. The controller still self-provisions everything it
needs in the target namespace (workload ServiceAccount, per-node check Jobs,
the stored report ConfigMap) exactly as it does when run via kubectl.

## What it deploys

- A controller **Job** running `rhaii-validator <checkMode>` in `namespace.name`.
- A **ServiceAccount** for that Job.
- A **ClusterRole + ClusterRoleBinding** granting the controller the API access
  it needs (see [RBAC](#rbac) — this is deliberately **not** cluster-admin).
- Optionally a **Namespace** (`namespace.create`) and a platform-config
  **ConfigMap** (`platformConfigYAML`).

## Quick start

```bash
# From the repo root. Runs the "all" check with upstream odh-stable images.
helm install rhaii-validate ./charts/rhaii-cluster-validation

# Follow the report/progress
kubectl logs -f job/rhaii-validate-rhaii-cluster-validation -n rhaii-validation

# Read the stored JSON report after completion
kubectl get cm rhaii-validate-report -n rhaii-validation \
  -o jsonpath='{.data.report\.json}' | jq .
```

Run a specific check and pin locally built images:

```bash
helm install rhaii-validate ./charts/rhaii-cluster-validation \
  --set checkMode=rdma \
  --set image.validator=quay.io/<user>/odh-rhaii-cluster-validator:latest \
  --set image.tools=quay.io/<user>/odh-rhaii-validator-tools:latest
```

On OpenShift (grants the controller `bind` on the privileged SCC so it can set
up check-Job host access):

```bash
helm install rhaii-validate ./charts/rhaii-cluster-validation \
  --set openshift.enabled=true
```

Uninstall (removes the Job, SA, and RBAC; see
[cleanup](#cleanup-vs-helm-uninstall)):

```bash
helm uninstall rhaii-validate
```

## Values

| Key | Default | Description |
|-----|---------|-------------|
| `namespace.name` | `rhaii-validation` | Namespace for all validation resources. |
| `namespace.create` | `true` | Create the namespace (controller also creates it if missing). |
| `checkMode` | `all` | Subcommand: `gpu`, `network`, `rdma`, `rdma-node`, `rdma-ping`, `rdma-bandwidth`, `all`, `deps`. |
| `image.validator` | `""` | Controller/validator image. Empty → `quay.io/opendatahub/odh-rhaii-cluster-validator:odh-stable`. |
| `image.tools` | `""` | Tools image (iperf3/RDMA). Empty → embedded default. |
| `image.pullPolicy` | `IfNotPresent` | Image pull policy for the controller Job. |
| `outputFormat` | `table` | Controller log output: `table` or `json`. |
| `timeout` | `""` | Check timeout, e.g. `5m`. Empty → built-in default. |
| `nodes` | `[]` | Restrict to specific GPU nodes. |
| `serverNode` / `clientNodes` | `""` / `[]` | Pin topology for network/rdma/bandwidth. |
| `pullSecret` | `""` | Existing image-pull Secret to attach to the workload SA. |
| `debug` | `false` | Keep check Jobs/pods alive after the run. |
| `platformConfigYAML` | `""` | Verbatim platform config written to the `rhaii-validate-config` ConfigMap. Ignored if that ConfigMap already exists. |
| `imagePullSecrets` | `[]` | Secrets to pull the controller image itself. |
| `controller.resources` | see values.yaml | Controller Job resource requests/limits. |
| `controller.nodeSelector` / `tolerations` / `affinity` | `{}` / `[]` / `{}` | Controller Job scheduling. |
| `controller.ttlSecondsAfterFinished` | `86400` | How long K8s keeps the finished Job. |
| `serviceAccount.create` | `true` | Create the controller ServiceAccount. |
| `serviceAccount.name` | `""` | SA name; defaults to the release fullname. |
| `rbac.create` | `true` | Create the controller ClusterRole/ClusterRoleBinding. |
| `openshift.enabled` | `false` | Add `bind` on `system:openshift:scc:privileged` to the controller role. |

## RBAC

The controller normally runs under your admin kubeconfig. In-cluster it needs a
ServiceAccount with the same access, so the chart grants a **scoped** ClusterRole
covering exactly the operations the controller performs — not cluster-admin:

- `namespaces`: get, create
- `nodes`: list (GPU/topology discovery)
- `pods` + `pods/log`: get, list, create (check pods and their JSON logs)
- `configmaps`: get, create, update, delete (config + stored report)
- `secrets`: get (optional image-pull secret)
- `serviceaccounts`: get, create, update, delete (workload SA for check Jobs)
- `jobs` (batch): get, list, create, delete
- `clusterroles` / `clusterrolebindings`: get, create, delete
- `clusterroles` **bind** — restricted via `resourceNames` to only the roles the
  controller binds (the chart role, `rhaii-validator`, and on OpenShift
  `system:openshift:scc:privileged`)
- `customresourcedefinitions` (apiextensions): get, list (deps checks)

Cluster-scoped permissions are unavoidable because the controller creates a
namespace, lists nodes, reads CRDs, and provisions a workload ServiceAccount
plus a (deliberately **empty**) ClusterRole/ClusterRoleBinding for check Jobs.
Creating an empty ClusterRole grants no permissions, so the controller needs
**no `escalate`** verb.

## Cleanup vs helm uninstall

`helm uninstall` removes the chart-managed resources (controller Job, SA, RBAC,
and any chart-created namespace/ConfigMap). It does **not** run the validator's
own `clean`, so resources the controller created at runtime — the workload
ServiceAccount, the per-run check Jobs, and the stored report/config
ConfigMaps — may remain. To remove those:

```bash
kubectl rhaii-validate clean -n rhaii-validation
# or delete the namespace entirely:
kubectl delete namespace rhaii-validation
```
