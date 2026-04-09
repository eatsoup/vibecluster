package cli

import (
	"fmt"
	"time"

	"github.com/eatsoup/vibecluster/pkg/k8s"
	"github.com/eatsoup/vibecluster/pkg/kubeconfig"
	"github.com/spf13/cobra"
)

type createOptions struct {
	connect         bool
	timeout         time.Duration
	print           bool
	syncerImage     string
	imagePullSecret string
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
	cmd.Flags().BoolVar(&opts.print, "print", false, "print kubeconfig to stdout instead of writing to file")
	cmd.Flags().StringVar(&opts.syncerImage, "syncer-image", "", "override the syncer container image (default: ghcr.io/eatsoup/vibecluster/syncer:latest)")
	cmd.Flags().StringVar(&opts.imagePullSecret, "image-pull-secret", "", "name of a dockerconfigjson secret (in default ns) to use for pulling images")

	return cmd
}

func runCreate(name string, opts *createOptions) error {
	client, restConfig, err := k8s.NewClient(kubeContext)
	if err != nil {
		return err
	}

	ctx := cmd_context()

	createOpts := k8s.CreateOptions{
		SyncerImage:     opts.syncerImage,
		ImagePullSecret: opts.imagePullSecret,
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

	if opts.connect {
		ns := k8s.NamespaceName(name)
		podName := name + "-0"

		fmt.Println("Setting up port-forward...")
		localPort, stopCh, err := k8s.PortForward(ctx, client, restConfig, ns, podName, k8s.K3sPort)
		if err != nil {
			return fmt.Errorf("port-forward: %w", err)
		}

		server := fmt.Sprintf("https://127.0.0.1:%d", localPort)

		fmt.Println("Retrieving kubeconfig...")
		config, err := kubeconfig.Retrieve(ctx, client, restConfig, name, server)
		if err != nil {
			close(stopCh)
			return fmt.Errorf("retrieving kubeconfig: %w", err)
		}

		if opts.print {
			close(stopCh)
			return kubeconfig.Print(config)
		}

		if err := kubeconfig.WriteToFile(config, ""); err != nil {
			close(stopCh)
			return fmt.Errorf("writing kubeconfig: %w", err)
		}

		contextName := "vibecluster-" + name
		fmt.Printf("Kubeconfig written. Current context set to %q\n", contextName)
		fmt.Printf("Port-forward running on port %d. Press Ctrl+C to stop.\n", localPort)
		fmt.Printf("\nTo use: kubectl get nodes\n")

		<-ctx.Done()
		close(stopCh)
	}

	return nil
}
