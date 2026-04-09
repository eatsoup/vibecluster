package cli

import (
	"fmt"

	"github.com/eatsoup/vibecluster/pkg/k8s"
	"github.com/spf13/cobra"
)

type operatorInstallOptions struct {
	image string
}

func newOperatorCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "operator",
		Short: "Manage the vibecluster operator",
		Long:  "Install or uninstall the vibecluster operator in your cluster for GitOps-based virtual cluster management.",
	}

	cmd.AddCommand(
		newOperatorInstallCommand(),
		newOperatorUninstallCommand(),
	)

	return cmd
}

func newOperatorInstallCommand() *cobra.Command {
	opts := &operatorInstallOptions{}

	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install the vibecluster operator",
		Long:  "Install the VirtualCluster CRD, RBAC, and operator Deployment into the cluster.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runOperatorInstall(opts)
		},
	}

	cmd.Flags().StringVar(&opts.image, "image", "", "override the operator container image (default: "+k8s.OperatorImage+")")

	return cmd
}

func newOperatorUninstallCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Uninstall the vibecluster operator",
		Long:  "Remove the operator Deployment, RBAC, namespace, and VirtualCluster CRD from the cluster.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runOperatorUninstall()
		},
	}

	return cmd
}

func runOperatorInstall(opts *operatorInstallOptions) error {
	client, restConfig, err := k8s.NewClient(kubeContext)
	if err != nil {
		return err
	}

	ctx := cmd_context()

	installOpts := k8s.OperatorInstallOptions{
		Image: opts.image,
	}

	fmt.Println("Installing vibecluster operator...")
	if err := k8s.InstallOperator(ctx, client, restConfig, installOpts); err != nil {
		return err
	}

	fmt.Println("Vibecluster operator installed successfully!")
	fmt.Println("You can now create virtual clusters using VirtualCluster custom resources.")
	return nil
}

func runOperatorUninstall() error {
	client, restConfig, err := k8s.NewClient(kubeContext)
	if err != nil {
		return err
	}

	ctx := cmd_context()

	fmt.Println("Uninstalling vibecluster operator...")
	if err := k8s.UninstallOperator(ctx, client, restConfig); err != nil {
		return err
	}

	fmt.Println("Vibecluster operator uninstalled successfully!")
	return nil
}
