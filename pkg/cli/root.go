package cli

import (
	"github.com/spf13/cobra"
)

var kubeContext string

func NewRootCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "vibecluster",
		Short: "Create and manage virtual Kubernetes clusters",
		Long:  "vibecluster creates lightweight virtual Kubernetes clusters inside a host cluster using k3s.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	cmd.PersistentFlags().StringVar(&kubeContext, "context", "", "kubernetes context to use")

	cmd.AddCommand(
		newCreateCommand(),
		newDeleteCommand(),
		newListCommand(),
		newConnectCommand(),
		newLogsCommand(),
	)

	return cmd
}
