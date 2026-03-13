package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/annotator"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks/gpu"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks/networking"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks/rdma"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/config"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/controller"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/runner"

	"github.com/spf13/cobra"
)

var version = "dev"

func main() {
	rootCmd := &cobra.Command{
		Use:           "rhaii-validate-agent",
		Short:         "RHAII cluster validation - GPU, RDMA, and network checks",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		Long: `RHAII cluster validation tool with two modes:

  run     - Agent mode: runs hardware checks on the current node (used by DaemonSet)
  deploy  - Controller mode: deploys agents to GPU nodes, collects results, prints report`,
	}

	rootCmd.AddCommand(newRunCmd())
	rootCmd.AddCommand(newDeployCmd())

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// --- run subcommand (agent mode) ---

func newRunCmd() *cobra.Command {
	var (
		nodeName     string
		bandwidth    bool
		iperfServer  string
		tcpThreshold float64
		configFile   string
		noWait       bool
	)

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run hardware checks on the current node (agent mode)",
		Long:  `Executes GPU, RDMA, and optional bandwidth checks on the current node. Outputs a JSON report to stdout.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if nodeName == "" {
				nodeName = os.Getenv("NODE_NAME")
			}
			if nodeName == "" {
				hostname, _ := os.Hostname()
				nodeName = hostname
			}

			ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()

			// Set up pod annotation updates (only when running in-cluster)
			setStatus := func(_ context.Context, _ string) {} // no-op for local runs
			if podName, podNS := os.Getenv("POD_NAME"), os.Getenv("POD_NAMESPACE"); podName != "" && podNS != "" {
				ann, err := annotator.New(podName, podNS)
				if err != nil {
					fmt.Fprintf(os.Stderr, "Warning: failed to create annotator: %v\n", err)
				} else {
					setStatus = func(ctx context.Context, status string) {
						if err := ann.SetStatus(ctx, status); err != nil {
							fmt.Fprintf(os.Stderr, "Warning: failed to set status %q: %v\n", status, err)
						}
					}
				}
			}

			setStatus(ctx, annotator.StatusStarting)

			// Load platform config from mounted ConfigMap or --config flag
			cfg, err := config.Load(config.PlatformUnknown, configFile)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to load config: %v, using defaults\n", err)
				cfg, _ = config.GetConfig(config.PlatformAKS) // fallback
			}
			fmt.Fprintf(os.Stderr, "Platform config: %s\n", cfg.Platform)

			r := runner.New(nodeName, os.Stdout)

			// GPU checks — use thresholds from config
			r.AddCheck(gpu.NewDriverCheck(nodeName, cfg.GPU.MinDriverVersion))
			r.AddCheck(gpu.NewECCCheck(nodeName))

			// RDMA checks
			r.AddCheck(rdma.NewDevicesCheck(nodeName))
			r.AddCheck(rdma.NewStatusCheck(nodeName))

			// Bandwidth checks (optional)
			if bandwidth {
				threshold := cfg.Thresholds.TCPBandwidth.Pass
				if cmd.Flags().Changed("tcp-threshold") {
					threshold = tcpThreshold // CLI flag explicitly set, overrides config
				}
				r.AddCheck(networking.NewTCPBandwidthCheck(nodeName, iperfServer, threshold))
			}

			setStatus(ctx, annotator.StatusRunning)

			report, err := r.Run(ctx)

			// Mark status so the controller knows the agent finished
			if err != nil {
				setStatus(ctx, annotator.StatusError)
			} else {
				setStatus(ctx, annotator.StatusDone)
			}

			hasFailures := err == nil && runner.HasFailures(report)

			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			} else if hasFailures {
				fmt.Fprintf(os.Stderr, "Validation failed: one or more checks reported FAIL\n")
			} else {
				fmt.Fprintf(os.Stderr, "Validation complete: all checks passed\n")
			}

			if noWait {
				// Exit immediately (for local/CI usage)
				if err != nil {
					return err
				}
				if hasFailures {
					return fmt.Errorf("validation failed: one or more checks reported FAIL")
				}
				return nil
			}

			// Block until terminated so the DaemonSet doesn't restart the container.
			// The controller collects logs via the "done" annotation, then cleans up.
			fmt.Fprintf(os.Stderr, "Waiting for controller to collect results...\n")
			<-ctx.Done()
			return nil
		},
	}

	cmd.Flags().StringVar(&nodeName, "node-name", "", "Node name (auto-detected from NODE_NAME env or hostname)")
	cmd.Flags().BoolVar(&bandwidth, "bandwidth", false, "Run bandwidth tests (iperf3, ib_write_bw)")
	cmd.Flags().StringVar(&iperfServer, "iperf-server", "", "IP address of iperf3 server for TCP bandwidth test")
	cmd.Flags().Float64Var(&tcpThreshold, "tcp-threshold", 25.0, "TCP bandwidth pass threshold in Gbps")
	cmd.Flags().StringVar(&configFile, "config", "", "Path to config file (overrides auto-loaded ConfigMap)")
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "Exit after checks instead of blocking (for local/CI usage)")

	return cmd
}

// --- deploy subcommand (controller mode) ---

func newDeployCmd() *cobra.Command {
	var opts controller.Options

	cmd := &cobra.Command{
		Use:   "deploy",
		Short: "Deploy agents to GPU nodes, collect results, print report (controller mode)",
		Long: `Discovers GPU nodes, detects the platform, creates a ConfigMap with platform defaults,
deploys a validation agent DaemonSet, waits for completion, collects results from pod logs,
prints a consolidated report, and cleans up.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.Image == "" {
				fmt.Fprintln(os.Stderr, "Error: --image is required")
				return fmt.Errorf("--image is required")
			}

			ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()

			ctrl, err := controller.New(opts, os.Stdout)
			if err != nil {
				return err
			}

			// Register multi-node jobs (run automatically when 2+ GPU nodes exist)
			ctrl.AddJob(networking.NewIperfJob(0, nil))      // threshold from config, PodConfig auto-detected
			ctrl.AddJob(rdma.NewRDMABandwidthJob(0, nil))   // threshold from config, PodConfig auto-detected

			return ctrl.Run(ctx)
		},
	}

	cmd.Flags().StringVar(&opts.Kubeconfig, "kubeconfig", "", "Path to kubeconfig (defaults to KUBECONFIG env or ~/.kube/config)")
	cmd.Flags().StringVar(&opts.Namespace, "namespace", "rhaii-validation", "Namespace for agent pods")
	cmd.Flags().StringVar(&opts.Image, "image", "", "Agent container image (required)")
	cmd.Flags().StringVar(&opts.ServerNode, "server-node", "", "Node to run bandwidth server on (default: first GPU node)")
	cmd.Flags().StringSliceVar(&opts.ClientNodes, "client-nodes", nil, "Nodes to run bandwidth clients on (default: all other GPU nodes)")
	cmd.Flags().DurationVar(&opts.Timeout, "timeout", 0, "Timeout waiting for agents (default 5m)")
	cmd.Flags().StringVar(&opts.ConfigFile, "config", "", "Path to config override (merged with detected platform defaults)")

	return cmd
}
