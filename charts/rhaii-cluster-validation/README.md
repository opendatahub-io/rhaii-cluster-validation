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

All resources are created in the Helm **release namespace** (set with
`--namespace <ns> --create-namespace`):

- A controller **Job** running `rhaii-validator <checkMode>`.
- A **ServiceAccount** for that Job.
- A **ClusterRole + ClusterRoleBinding** granting the controller the API access
  it needs (see [RBAC](#rbac) — this is deliberately **not** cluster-admin).
- Optionally a platform-config **ConfigMap** (`platformConfigYAML`) and an image
  **pull Secret** (`pullSecret`).

## Quick start

Install from an OCI registry (once published):

```bash
helm upgrade --install rhaii oci://quay.io/aneeshkp/charts/rhaii-cluster-validation \
  --version 0.1.0 \
  --namespace rhaii-validation --create-namespace
```

Or from a local checkout:

```bash
helm upgrade --install rhaii ./charts/rhaii-cluster-validation \
  --namespace rhaii-validation --create-namespace
```

Then watch it:

```bash
kubectl logs -f job/rhaii-rhaii-cluster-validation -n rhaii-validation
kubectl get cm rhaii-validate-report -n rhaii-validation \
  -o jsonpath='{.data.report\.json}' | jq .
```

Run a specific check and pin locally built images:

```bash
helm upgrade --install rhaii ./charts/rhaii-cluster-validation \
  --namespace rhaii-validation --create-namespace \
  --set checkMode=rdma \
  --set image.validator=quay.io/<user>/odh-rhaii-cluster-validator:latest \
  --set image.tools=quay.io/<user>/odh-rhaii-validator-tools:latest
```

On OpenShift (grants the controller `bind` on the privileged SCC so it can set
up check-Job host access):

```bash
helm upgrade --install rhaii ./charts/rhaii-cluster-validation \
  --namespace rhaii-validation --create-namespace \
  --set openshift.enabled=true
```

> Note: the `deps` check is API-only and always uses the `rhaii-validation`
> namespace for its stored report (it doesn't take `--namespace`). For `deps`,
> install into `--namespace rhaii-validation` so everything lines up.

### Pulling images from registry.redhat.io

The Red Hat catalog images (and any private registry) need a pull secret. The
chart applies one secret to both the controller image and the check Jobs.

Easiest — hand the chart your docker login and it creates the secret for you:

```bash
podman login registry.redhat.io   # writes ${XDG_RUNTIME_DIR}/containers/auth.json

helm install rhaii-validate ./charts/rhaii-cluster-validation \
  --set image.validator=registry.redhat.io/rhoai/odh-rhaii-cluster-validator-rhel9:v3.4.0 \
  --set image.tools=registry.redhat.io/rhoai/odh-rhaii-validator-tools-rhel9:v3.4.0 \
  --set-file pullSecret.dockerConfigJson=${XDG_RUNTIME_DIR}/containers/auth.json
```

Or reference a secret you already created:

```bash
kubectl create namespace rhaii-validation
kubectl create secret docker-registry rhaii-pull \
  --docker-server=registry.redhat.io \
  --docker-username='<user>' --docker-password='<token>' \
  -n rhaii-validation

helm upgrade --install rhaii ./charts/rhaii-cluster-validation \
  --namespace rhaii-validation --create-namespace \
  --set image.validator=registry.redhat.io/rhoai/odh-rhaii-cluster-validator-rhel9:v3.4.0 \
  --set image.tools=registry.redhat.io/rhoai/odh-rhaii-validator-tools-rhel9:v3.4.0 \
  --set pullSecret.name=rhaii-pull
```

Uninstall (removes the Job, SA, and RBAC; see
[cleanup](#cleanup-vs-helm-uninstall)):

```bash
helm uninstall rhaii -n rhaii-validation
```

## Values

Resources are created in the Helm release namespace (`--namespace`), so there is
no namespace value.

| Key | Default | Description |
|-----|---------|-------------|
| `checkMode` | `all` | Subcommand: `gpu`, `network`, `rdma`, `rdma-node`, `rdma-ping`, `rdma-bandwidth`, `all`, `deps`. |
| `image.validator` | `""` | Controller/validator image. Empty → `quay.io/opendatahub/odh-rhaii-cluster-validator:odh-stable`. |
| `image.tools` | `""` | Tools image (iperf3/RDMA). Empty → embedded default. |
| `image.pullPolicy` | `IfNotPresent` | Image pull policy for the controller Job. |
| `outputFormat` | `table` | Controller log output: `table` or `json`. |
| `timeout` | `""` | Check timeout, e.g. `5m`. Empty → built-in default. |
| `nodes` | `[]` | Restrict to specific GPU nodes. |
| `serverNode` / `clientNodes` | `""` / `[]` | Pin topology for network/rdma/bandwidth. |
| `pullSecret.dockerConfigJson` | `""` | docker config.json contents. When set, the chart **creates** a pull Secret and applies it to BOTH the controller Job (`imagePullSecrets`) and the check Jobs (via `--pull-secret`). Use `--set-file`. |
| `pullSecret.name` | `""` | Reference an existing docker-registry Secret (when `dockerConfigJson` is empty), or override the generated name (default `<release>-pull`) when creating. |
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

`helm uninstall rhaii -n <ns>` removes the chart-managed resources (controller
Job, SA, RBAC, ConfigMap). It does **not** run the validator's own `clean`, so
resources the controller created at runtime — the workload ServiceAccount, the
per-run check Jobs, and the stored report/config ConfigMaps — may remain. To
remove those:

```bash
kubectl rhaii-validate clean -n <ns>
# or delete the namespace entirely:
kubectl delete namespace <ns>
```
