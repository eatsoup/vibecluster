package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/eatsoup/vibecluster/pkg/k8s"
	"github.com/eatsoup/vibecluster/pkg/kubeconfig"
	"github.com/spf13/cobra"
	"k8s.io/client-go/rest"
)

type createOptions struct {
	connect            bool
	timeout            time.Duration
	updateKubeconfig   bool
	kubeconfigOut      string
	print              bool
	syncerImage        string
	imagePullSecret    string
	exposeType         string
	exposeIngressClass string
	exposeHost         string
	mode               string
	crNamespace        string
	cpu                string
	memory             string
	storage            string
	pods               int32
	vnode              bool
}

func newCreateCommand() *cobra.Command {
	opts := &createOptions{}

	cmd := &cobra.Command{
		Use:   "create NAME",
		Short: "Create a virtual cluster",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCreate(args[0], opts)
		},
	}

	cmd.Flags().BoolVar(&opts.connect, "connect", true, "connect to the virtual cluster after creation")
	cmd.Flags().DurationVar(&opts.timeout, "timeout", 5*time.Minute, "timeout waiting for the virtual cluster to be ready")
	cmd.Flags().BoolVar(&opts.updateKubeconfig, "update-kubeconfig", false, "merge the virtual cluster into your default kubeconfig")
	cmd.Flags().StringVar(&opts.kubeconfigOut, "kubeconfig", "", "write kubeconfig to this file (default: ./vibecluster-<name>.kubeconfig)")
	cmd.Flags().BoolVar(&opts.print, "print", false, "print kubeconfig to stdout instead of writing to file")
	cmd.Flags().StringVar(&opts.syncerImage, "syncer-image", "", "override the syncer container image (default: ghcr.io/eatsoup/vibecluster/syncer:latest)")
	cmd.Flags().StringVar(&opts.imagePullSecret, "image-pull-secret", "", "name of a dockerconfigjson secret (in default ns) to use for pulling images")
	cmd.Flags().StringVar(&opts.exposeType, "expose", "", "exposure type for the cluster (LoadBalancer, Ingress)")
	cmd.Flags().StringVar(&opts.exposeIngressClass, "expose-ingress-class", "", "ingress class if expose is Ingress")
	cmd.Flags().StringVar(&opts.exposeHost, "expose-host", "", "ingress hostname if expose is Ingress")
	cmd.Flags().StringVar(&opts.mode, "mode", "auto", "creation mode: auto (use operator if available), legacy (raw manifests), or operator (require CRD)")
	cmd.Flags().StringVar(&opts.crNamespace, "cr-namespace", k8s.DefaultCROperatorNamespace, "namespace to create the VirtualCluster CR in (operator mode only)")
	cmd.Flags().StringVar(&opts.cpu, "cpu", "", "CPU budget for the virtual cluster (e.g. 4, 500m); enforced via a namespace ResourceQuota. Includes the k3s control plane.")
	cmd.Flags().StringVar(&opts.memory, "memory", "", "memory budget for the virtual cluster (e.g. 8Gi); enforced via a namespace ResourceQuota. Includes the k3s control plane.")
	cmd.Flags().StringVar(&opts.storage, "storage", "", "total persistent storage budget across all PVCs (e.g. 50Gi); enforced via a namespace ResourceQuota.")
	cmd.Flags().Int32Var(&opts.pods, "pods", 0, "maximum pod count in the virtual cluster (0 = unlimited).")
	cmd.Flags().BoolVar(&opts.vnode, "vnode", false, "prototype: run a nested k3s agent pod so NetworkPolicy and LoadBalancer work inside the virtual cluster. Requires privileged pods on the host. See issue #27.")

	return cmd
}

// resourceLimitsFromOpts builds the per-vcluster resource budget from CLI
// flags, returning nil when none of them were set.
func resourceLimitsFromOpts(opts *createOptions) *k8s.ResourceLimits {
	if opts.cpu == "" && opts.memory == "" && opts.storage == "" && opts.pods == 0 {
		return nil
	}
	return &k8s.ResourceLimits{
		CPU:     opts.cpu,
		Memory:  opts.memory,
		Storage: opts.storage,
		Pods:    opts.pods,
	}
}

func runCreate(name string, opts *createOptions) error {
	client, restConfig, err := k8s.NewClient(kubeContext)
	if err != nil {
		return err
	}

	ctx := cmd_context()

	useOperator, err := resolveCreateMode(ctx, restConfig, opts.mode)
	if err != nil {
		return err
	}

	if opts.vnode && useOperator {
		// The CRD does not model the vnode prototype field yet (issue #27).
		// Forcing legacy keeps the flag usable without a CRD version bump
		// while the prototype is still on a branch.
		return fmt.Errorf("--vnode is not yet supported in operator mode; re-run with --mode=legacy")
	}

	if useOperator {
		fmt.Printf("Operator detected. Creating VirtualCluster CR %q in namespace %q...\n", name, opts.crNamespace)
		if opts.imagePullSecret != "" {
			fmt.Println("  Note: --image-pull-secret is ignored in operator mode (CRD does not yet model it).")
		}
		spec := k8s.VirtualClusterCRSpec{
			SyncerImage: opts.syncerImage,
		}
		if opts.exposeType != "" {
			spec.Expose = &k8s.VirtualClusterCRExpose{
				Type:         opts.exposeType,
				Host:         opts.exposeHost,
				IngressClass: opts.exposeIngressClass,
			}
		} else if opts.exposeHost != "" || opts.exposeIngressClass != "" {
			return fmt.Errorf("--expose-host/--expose-ingress-class require --expose=Ingress or --expose=LoadBalancer")
		}
		if rl := resourceLimitsFromOpts(opts); rl != nil {
			spec.Resources = rl
		}
		if err := k8s.CreateVirtualClusterCR(ctx, restConfig, name, opts.crNamespace, spec); err != nil {
			return fmt.Errorf("creating VirtualCluster CR: %w", err)
		}
		fmt.Println("VirtualCluster CR created. The operator will reconcile it; check 'vibecluster list' for status.")
		return nil
	}

	createOpts := k8s.CreateOptions{
		SyncerImage:        opts.syncerImage,
		ImagePullSecret:    opts.imagePullSecret,
		ExposeType:         opts.exposeType,
		ExposeIngressClass: opts.exposeIngressClass,
		ExposeHost:         opts.exposeHost,
		Resources:          resourceLimitsFromOpts(opts),
		VNode:              opts.vnode,
	}

	fmt.Printf("Creating virtual cluster %q...\n", name)
	if err := k8s.CreateVirtualCluster(ctx, client, name, createOpts); err != nil {
		return err
	}

	fmt.Printf("Waiting for virtual cluster to be ready (timeout: %s)...\n", opts.timeout)
	if err := k8s.WaitForReady(ctx, client, name, opts.timeout); err != nil {
		return err
	}

	fmt.Println("Virtual cluster is ready!")

	if !opts.connect {
		return nil
	}

	// If --expose was set, wait for the external address and point the kubeconfig at it.
	retrieveOpts := kubeconfig.RetrieveOptions{}
	if opts.exposeType != "" {
		fmt.Printf("Waiting for %s address (timeout: %s)...\n", opts.exposeType, opts.timeout)
		addr, waitErr := k8s.WaitForExternalAddress(ctx, client, name, opts.timeout)
		if waitErr != nil {
			return fmt.Errorf("waiting for external address: %w", waitErr)
		}
		retrieveOpts.Server = addr.URL()
		retrieveOpts.InsecureSkipTLSVerify = !addr.CertVerifies
		fmt.Printf("External address: %s\n", retrieveOpts.Server)
	}

	// Retrieve kubeconfig without standing up a port-forward.
	// RetrieveWithOptions uses the host apiserver for `exec`, so no port-forward is required.
	fmt.Println("Retrieving kubeconfig...")
	config, err := kubeconfig.RetrieveWithOptions(ctx, client, restConfig, name, retrieveOpts)
	if err != nil {
		return fmt.Errorf("retrieving kubeconfig: %w", err)
	}

	if opts.print {
		return kubeconfig.Print(config)
	}

	outPath := opts.kubeconfigOut
	if outPath == "" && !opts.updateKubeconfig {
		outPath = fmt.Sprintf("./vibecluster-%s.kubeconfig", name)
	}

	if err := kubeconfig.WriteToFile(config, outPath); err != nil {
		return fmt.Errorf("writing kubeconfig: %w", err)
	}

	contextName := "vibecluster-" + name
	if opts.updateKubeconfig {
		fmt.Printf("Kubeconfig updated in default location. Current context set to %q\n", contextName)
	} else {
		fmt.Printf("Kubeconfig written to %s\n", outPath)
		fmt.Printf("To use: export KUBECONFIG=%s\n", outPath)
	}

	if opts.exposeType == "" {
		fmt.Println("\nThe kubeconfig points to the in-cluster service URL. To reach the cluster from your machine:")
		fmt.Printf("  vibecluster expose %s --temp                # ephemeral port-forward\n", name)
		fmt.Printf("  vibecluster expose %s --type LoadBalancer   # persistent external IP\n", name)
	}

	return nil
}

// resolveCreateMode decides whether to use the operator CR path based on the
// requested mode and whether the CRD is installed in the host cluster.
func resolveCreateMode(ctx context.Context, restConfig *rest.Config, mode string) (bool, error) {
	// Avoid checking operator availability when the mode answer doesn't depend on it.
	if mode == "legacy" {
		return decideCreateMode(mode, false)
	}
	available, err := k8s.IsOperatorAvailable(ctx, restConfig)
	if err != nil {
		return false, fmt.Errorf("checking operator availability: %w", err)
	}
	return decideCreateMode(mode, available)
}

// decideCreateMode is the pure decision portion of resolveCreateMode and is
// extracted so it can be unit-tested without a live host cluster.
func decideCreateMode(mode string, operatorAvailable bool) (bool, error) {
	switch mode {
	case "legacy":
		return false, nil
	case "operator":
		if !operatorAvailable {
			return false, fmt.Errorf("--mode=operator requested but the VirtualCluster CRD is not installed (run `vibecluster operator install`)")
		}
		return true, nil
	case "auto", "":
		return operatorAvailable, nil
	default:
		return false, fmt.Errorf("invalid --mode %q (must be auto, legacy, or operator)", mode)
	}
}
