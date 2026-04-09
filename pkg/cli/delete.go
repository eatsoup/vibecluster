package cli

import (
	"fmt"

	"github.com/eatsoup/vibecluster/pkg/k8s"
	"github.com/eatsoup/vibecluster/pkg/kubeconfig"
	"github.com/spf13/cobra"
)

func newDeleteCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "delete NAME",
		Short: "Delete a virtual cluster",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDelete(args[0])
		},
	}
}

func runDelete(name string) error {
	client, _, err := k8s.NewClient(kubeContext)
	if err != nil {
		return err
	}

	ctx := cmd_context()

	fmt.Printf("Deleting virtual cluster %q...\n", name)
	if err := k8s.DeleteVirtualCluster(ctx, client, name); err != nil {
		return err
	}

	// Clean up kubeconfig
	if err := kubeconfig.RemoveFromFile(name, ""); err != nil {
		fmt.Printf("Warning: failed to clean kubeconfig: %v\n", err)
	}

	fmt.Printf("Virtual cluster %q deleted.\n", name)
	return nil
}
