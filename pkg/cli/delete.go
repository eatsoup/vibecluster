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
	client, restConfig, err := k8s.NewClient(kubeContext)
	if err != nil {
		return err
	}

	ctx := cmd_context()

	// If a VirtualCluster CR exists for this name, delete via the operator path so the operator
	// reconciles cleanup. Falls back to the legacy raw-manifest delete if no CR is found.
	if available, err := k8s.IsOperatorAvailable(ctx, restConfig); err == nil && available {
		crNS, err := k8s.FindVirtualClusterCR(ctx, restConfig, name)
		if err != nil {
			return fmt.Errorf("looking up VirtualCluster CR: %w", err)
		}
		if crNS != "" {
			fmt.Printf("Deleting VirtualCluster CR %s/%s...\n", crNS, name)
			if _, err := k8s.DeleteVirtualClusterCR(ctx, restConfig, name, crNS); err != nil {
				return fmt.Errorf("deleting VirtualCluster CR: %w", err)
			}
			if err := kubeconfig.RemoveFromFile(name, ""); err != nil {
				fmt.Printf("Warning: failed to clean kubeconfig: %v\n", err)
			}
			fmt.Printf("Virtual cluster %q deletion requested. The operator will finish cleanup.\n", name)
			return nil
		}
	}

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
