# Developer Guide: Integrating with odh-cli

This document describes how to import the `rhaii-cluster-validation` packages into `odh-cli` to power the `kubectl odh validate --extended` command.

## Architecture

```
odh-cli (consumer)                      rhaii-cluster-validation (library)
├── cmd/validate/validate.go            ├── pkg/checks/          (Check interface + implementations)
│   └── imports pkg/controller          ├── pkg/config/          (Platform detection + config)
│   └── imports pkg/checks              ├── pkg/controller/      (DaemonSet lifecycle + result collection)
│   └── imports pkg/config              ├── pkg/runner/          (Check execution engine)
│                                       ├── pkg/annotator/       (Pod annotation status updates)
│                                       └── deploy/              (Embedded DaemonSet + RBAC YAML)
```

## Step 1: Add the dependency

```bash
cd odh-cli
go get github.com/opendatahub-io/rhaii-cluster-validation@latest
```

## Step 2: Use the controller package

The `pkg/controller` package handles the full agent lifecycle. Import it into a new `validate` command in odh-cli:

```go
package validate

import (
    "context"
    "os"

    "github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks/networking"
    "github.com/opendatahub-io/rhaii-cluster-validation/pkg/controller"
    "github.com/spf13/cobra"
)

func NewCommand() *cobra.Command {
    var opts controller.Options

    cmd := &cobra.Command{
        Use:   "validate",
        Short: "Validate cluster readiness for llm-d deployment",
        RunE: func(cmd *cobra.Command, args []string) error {
            ctx := cmd.Context()

            // Controller handles: discover GPU nodes → deploy DaemonSet →
            // wait → collect pod logs → cleanup → print report
            ctrl, err := controller.New(opts, os.Stdout)
            if err != nil {
                return err
            }

            // Register multi-node jobs (run automatically when 2+ GPU nodes)
            ctrl.AddJob(networking.NewIperfJob(0, nil))

            return ctrl.Run(ctx)
        },
    }

    cmd.Flags().StringVar(&opts.Image, "image", "", "Agent container image")
    cmd.Flags().StringVar(&opts.Namespace, "namespace", "rhaii-validation", "Agent namespace")
    cmd.Flags().DurationVar(&opts.Timeout, "timeout", 0, "Timeout (default 5m)")
    cmd.Flags().StringVar(&opts.ConfigFile, "config", "", "Platform config override")
    cmd.Flags().StringVar(&opts.ServerNode, "server-node", "", "Bandwidth server node")
    cmd.Flags().StringSliceVar(&opts.ClientNodes, "client-nodes", nil, "Bandwidth client nodes")

    return cmd
}
```

Then register in odh-cli's root command:

```go
// cmd/main.go (odh-cli)
import "github.com/opendatahub-io/odh-cli/cmd/validate"

rootCmd.AddCommand(validate.NewCommand())
```

## Step 3: Use the checks package directly

If odh-cli wants to run checks without the controller (e.g., as part of `lint`), import the checks directly:

```go
package lint

import (
    "context"

    "github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
    "github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks/gpu"
    "github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks/rdma"
)

func runGPUChecks(ctx context.Context, nodeName string) []checks.Result {
    var results []checks.Result

    allChecks := []checks.Check{
        gpu.NewDriverCheck(nodeName, "535.0"),
        gpu.NewECCCheck(nodeName),
        rdma.NewDevicesCheck(nodeName),
        rdma.NewStatusCheck(nodeName),
    }

    for _, c := range allChecks {
        results = append(results, c.Run(ctx))
    }

    return results
}
```

## Step 4: Use platform detection

```go
package validate

import (
    "context"

    "github.com/opendatahub-io/rhaii-cluster-validation/pkg/config"
    "k8s.io/client-go/kubernetes"
)

func detectAndLoadConfig(ctx context.Context, client kubernetes.Interface, configFile string) (config.PlatformConfig, error) {
    // Auto-detect platform from node labels
    platform := config.DetectPlatform(ctx, client)

    // Load embedded defaults + optional override file
    // Also checks /etc/rhaii-validate/platform.yaml automatically
    cfg, err := config.Load(platform, configFile)
    if err != nil {
        return config.PlatformConfig{}, err
    }

    return cfg, nil
}
```

## Step 5: Convert results to odh-cli DiagnosticResult

odh-cli uses its own `DiagnosticResult` format. Map the agent's `checks.Result` to it:

```go
package validate

import (
    agentchecks "github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
    "github.com/opendatahub-io/odh-cli/pkg/lint/check/result"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func toDiagnosticResult(r agentchecks.Result) *result.DiagnosticResult {
    dr := &result.DiagnosticResult{
        ObjectMeta: metav1.ObjectMeta{
            Name: r.Name,
            Annotations: map[string]string{
                "node": r.Node,
            },
        },
    }

    // Map PASS/WARN/FAIL/SKIP to K8s condition status
    var status metav1.ConditionStatus
    switch r.Status {
    case agentchecks.StatusPass:
        status = metav1.ConditionTrue
    case agentchecks.StatusFail:
        status = metav1.ConditionFalse
    case agentchecks.StatusWarn, agentchecks.StatusSkip:
        status = metav1.ConditionUnknown
    }

    dr.Status.Conditions = []metav1.Condition{{
        Type:    r.Category + "/" + r.Name,
        Status:  status,
        Reason:  string(r.Status),
        Message: r.Message,
    }}

    return dr
}
```

## Adding New Checks and Jobs

There are two kinds of validation checks:

| Type | Scope | Runs as | Example |
|---|---|---|---|
| **Per-node check** | Single node | DaemonSet agent pod | GPU driver, ECC, RDMA devices |
| **Multi-node job** | Between nodes | K8s Jobs (server + clients) | iperf3, ib_write_bw, NCCL |

### Adding a new per-node check

1. Create a file in the appropriate category under `pkg/checks/`:

```go
// pkg/checks/gpu/topology.go
package gpu

import (
    "context"
    "github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
)

type TopologyCheck struct {
    nodeName string
}

func NewTopologyCheck(nodeName string) *TopologyCheck {
    return &TopologyCheck{nodeName: nodeName}
}

func (c *TopologyCheck) Name() string     { return "gpu_nic_topology" }
func (c *TopologyCheck) Category() string { return "gpu_hardware" }

func (c *TopologyCheck) Run(ctx context.Context) checks.Result {
    r := checks.Result{
        Node:     c.nodeName,
        Category: c.Category(),
        Name:     c.Name(),
    }
    // ... run nvidia-smi topo -m, parse output
    r.Status = checks.StatusPass
    r.Message = "GPU-NIC topology is 1:1"
    return r
}
```

2. Register it in `cmd/agent/main.go` in the `run` subcommand:

```go
r.AddCheck(gpu.NewTopologyCheck(nodeName))
```

3. Add a `_test.go` file with parsed output tests.

### Adding a new multi-node job

1. Create a file in the appropriate category under `pkg/checks/`:

```go
// pkg/checks/gpu/nccl_job.go
package gpu

import (
    "github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
    "github.com/opendatahub-io/rhaii-cluster-validation/pkg/jobrunner"
    batchv1 "k8s.io/api/batch/v1"
)

type NCCLJob struct {
    PodCfg *jobrunner.PodConfig
}

func NewNCCLJob(podCfg *jobrunner.PodConfig) *NCCLJob {
    return &NCCLJob{PodCfg: podCfg}
}

func (j *NCCLJob) Name() string { return "nccl-allreduce" }

func (j *NCCLJob) SetPodConfig(cfg *jobrunner.PodConfig) { j.PodCfg = cfg }

func (j *NCCLJob) ServerSpec(node, namespace, image string) (*batchv1.Job, error) {
    return jobrunner.BuildJobSpec(j.Name(), node, namespace, image,
        jobrunner.RoleServer, j.PodCfg,
        []string{"nccl-test", "--server"})
}

func (j *NCCLJob) ClientSpec(node, namespace, image, serverIP string) (*batchv1.Job, error) {
    return jobrunner.BuildJobSpec(j.Name(), node, namespace, image,
        jobrunner.RoleClient, j.PodCfg,
        []string{"nccl-test", "--client", serverIP})
}

func (j *NCCLJob) ParseResult(logs string) (*jobrunner.JobResult, error) {
    // parse nccl-test output for bandwidth
    return &jobrunner.JobResult{
        Status:  checks.StatusPass,
        Message: "NCCL all-reduce: 400 Gbps",
    }, nil
}
```

2. Register it in `cmd/agent/main.go` in the `deploy` subcommand:

```go
ctrl.AddJob(gpu.NewNCCLJob(nil))  // PodConfig auto-detected (GPU resources)
```

That's it. The controller automatically:
- Runs the job when 2+ GPU nodes exist
- Auto-detects GPU resource name and count from nodes
- Sets PodConfig on jobs that implement `Configurable`
- Deploys server Job on one node, client Jobs on the rest
- Collects logs, parses results, includes in the report

## Importable Packages Summary

| Package | What it provides | Use in odh-cli |
|---|---|---|
| `pkg/checks` | `Check` interface, `Result`, `NodeReport` types | Define check contract |
| `pkg/checks/gpu` | GPU driver, ECC checks | Run GPU checks directly or via agent |
| `pkg/checks/rdma` | RDMA device, NIC status checks + ib_write_bw job | Run RDMA checks directly or via agent |
| `pkg/checks/networking` | TCP bandwidth check + iperf3 job | Run network checks directly or via agent |
| `pkg/config` | Platform detection, config loading | Auto-detect platform, load thresholds |
| `pkg/controller` | Full lifecycle: RBAC, DaemonSet, jobs, pod log collection, report | Full `validate --extended` workflow |
| `pkg/runner` | Execute checks, output JSON, return report with failure detection | Run checks in agent mode |
| `pkg/jobrunner` | Job interface, Runner (server/client lifecycle), PodConfig | Multi-node test orchestration |
| `pkg/annotator` | Pod annotation updates (`NewWithClient` for DI) | Track agent progress |
| `deploy` | Embedded DaemonSet + RBAC YAML | Single source of truth for manifests |

### Key Design Notes

- **`deploy` command is self-contained** — creates RBAC, ConfigMap, DaemonSet, and cleans up everything after collection. No manual `kubectl apply` needed.
- **Annotation-based completion** — agents set `rhaii.opendatahub.io/validation-status` annotation (`starting` → `running` → `done`/`error`). Controller watches for `done`/`error`.
- **Agents block after checks** — `<-ctx.Done()` keeps the container alive so the controller can read logs. Controller cleanup terminates the pods.
- **Exit codes** — both `run --no-wait` and `deploy` exit non-zero if any check reported FAIL, enabling CI/CD gating.
- **Embedded manifests** — `deploy/daemonset.yaml` and `deploy/rbac.yaml` are embedded via `//go:embed` and used by the controller. Same files used by `make run` via kubectl. Single source of truth.
- **Testability** — `NewWithClient()` constructors accept injected `kubernetes.Interface` for unit testing with `fake.NewSimpleClientset()`.

## Testing Integration

```go
package validate_test

import (
    "context"
    "testing"

    "github.com/opendatahub-io/rhaii-cluster-validation/pkg/config"
)

func TestPlatformDetection(t *testing.T) {
    // Use fake K8s client to test detection
    // See pkg/config/detect_test.go for examples
}

func TestConfigLoading(t *testing.T) {
    cfg, err := config.Load(config.PlatformAKS, "")
    if err != nil {
        t.Fatal(err)
    }
    if cfg.GPU.TaintKey != "sku" {
        t.Errorf("expected AKS taint key 'sku', got %s", cfg.GPU.TaintKey)
    }
}
```

## Version Compatibility

When updating `rhaii-cluster-validation`, ensure:

1. `Check` interface hasn't changed (breaking change for all consumers)
2. `Result` struct fields are backward compatible (add fields, don't remove)
3. `controller.Options` is backward compatible
4. Platform config YAML schema is backward compatible
5. Run `go mod tidy` in odh-cli after updating the dependency
