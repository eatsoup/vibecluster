package cli

import (
	"fmt"
	"os"

	"github.com/eatsoup/vibecluster/pkg/k8s"
	"github.com/eatsoup/vibecluster/pkg/kubeconfig"
	"github.com/spf13/cobra"
)

type connectOptions struct {
	server     string
	print      bool
	outputFile string
}

func newConnectCommand() *cobra.Command {
	opts := &connectOptions{}

	cmd := &cobra.Command{
		Use:   "connect NAME",
		Short: "Connect to a virtual cluster (update kubeconfig)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConnect(args[0], opts)
		},
	}

	cmd.Flags().StringVar(&opts.server, "server", "", "override the API server address in the kubeconfig (default: sets up port-forward)")
	cmd.Flags().BoolVar(&opts.print, "print", false, "print kubeconfig to stdout instead of writing to file")
	cmd.Flags().StringVar(&opts.outputFile, "kubeconfig", "", "write kubeconfig to this file (default: ~/.kube/config)")

	return cmd
}

func runConnect(name string, opts *connectOptions) error {
	client, restConfig, err := k8s.NewClient(kubeContext)
	if err != nil {
		return err
	}

	ctx := cmd_context()

	// Check the cluster exists and is ready
	if err := k8s.WaitForReady(ctx, client, name, 30e9); err != nil {
		return fmt.Errorf("virtual cluster %q is not ready: %w", name, err)
	}

	server := opts.server
	var stopCh chan struct{}

	// If no server override, set up port-forward for local access
	if server == "" {
		ns := k8s.NamespaceName(name)
		podName := name + "-0"

		fmt.Fprintf(os.Stderr, "Setting up port-forward to %s/%s...\n", ns, podName)
		localPort, stop, err := k8s.PortForward(ctx, client, restConfig, ns, podName, k8s.K3sPort)
		if err != nil {
			return fmt.Errorf("port-forward: %w", err)
		}
		stopCh = stop
		server = fmt.Sprintf("https://127.0.0.1:%d", localPort)
		fmt.Fprintf(os.Stderr, "Port-forward established on port %d\n", localPort)
	}

	fmt.Fprintf(os.Stderr, "Retrieving kubeconfig for %q...\n", name)
	config, err := kubeconfig.Retrieve(ctx, client, restConfig, name, server)
	if err != nil {
		if stopCh != nil {
			close(stopCh)
		}
		return err
	}

	if opts.print {
		if stopCh != nil {
			close(stopCh)
		}
		return kubeconfig.Print(config)
	}

	if err := kubeconfig.WriteToFile(config, opts.outputFile); err != nil {
		if stopCh != nil {
			close(stopCh)
		}
		return err
	}

	contextName := "vibecluster-" + name
	fmt.Fprintf(os.Stderr, "Kubeconfig written. Current context set to %q\n", contextName)

	if stopCh != nil {
		fmt.Fprintf(os.Stderr, "Port-forward running in background. Press Ctrl+C to stop.\n")
		fmt.Fprintf(os.Stderr, "\nTo use: kubectl get nodes\n")
		<-ctx.Done()
		close(stopCh)
	}

	return nil
}
