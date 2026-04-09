package cli

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/eatsoup/vibecluster/pkg/k8s"
	"github.com/spf13/cobra"
)

func newListCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List virtual clusters",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runList()
		},
	}
}

func runList() error {
	client, _, err := k8s.NewClient(kubeContext)
	if err != nil {
		return err
	}

	ctx := cmd_context()

	clusters, err := k8s.ListVirtualClusters(ctx, client)
	if err != nil {
		return err
	}

	if len(clusters) == 0 {
		fmt.Println("No virtual clusters found.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "NAME\tNAMESPACE\tSTATUS\tCREATED")
	for _, c := range clusters {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", c.Name, c.Namespace, c.Status, c.Created)
	}
	return w.Flush()
}
