package cli

import (
	"fmt"
	"time"

	"github.com/eatsoup/vibecluster/pkg/k8s"
	"github.com/eatsoup/vibecluster/pkg/kubeconfig"
	"github.com/spf13/cobra"
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

	return cmd
}

func runCreate(name string, opts *createOptions) error {
	client, restConfig, err := k8s.NewClient(kubeContext)
	if err != nil {
		return err
	}

	ctx := cmd_context()

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

	// Retrieve kubeconfig without standing up a port-forward.
	// kubeconfig.Retrieve uses the host apiserver for `exec`, so no port-forward is required.
	// The server URL falls back to the in-cluster service address; use `vibecluster expose --temp`
	// or `--expose <type>` to make the API reachable from outside the cluster.
	fmt.Println("Retrieving kubeconfig...")
	config, err := kubeconfig.Retrieve(ctx, client, restConfig, name, "")
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

	fmt.Println("\nThe kubeconfig points to the in-cluster service URL. To reach the cluster from your machine:")
	fmt.Printf("  vibecluster expose %s --temp                # ephemeral port-forward\n", name)
	fmt.Printf("  vibecluster expose %s --type LoadBalancer   # persistent external IP\n", name)

	return nil
}
