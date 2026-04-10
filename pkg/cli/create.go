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

	return cmd
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

	if useOperator {
		fmt.Printf("Operator detected. Creating VirtualCluster CR %q in namespace %q...\n", name, opts.crNamespace)
		if opts.exposeType != "" || opts.exposeHost != "" || opts.exposeIngressClass != "" {
			fmt.Println("  Note: --expose flags are ignored in operator mode (CRD does not yet model expose).")
		}
		if opts.imagePullSecret != "" {
			fmt.Println("  Note: --image-pull-secret is ignored in operator mode (CRD does not yet model it).")
		}
		spec := k8s.VirtualClusterCRSpec{
			SyncerImage: opts.syncerImage,
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
