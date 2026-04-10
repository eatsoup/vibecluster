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
	client, restConfig, err := k8s.NewClient(kubeContext)
	if err != nil {
		return err
	}

	ctx := cmd_context()

	clusters, err := k8s.ListVirtualClusters(ctx, client)
	if err != nil {
		return err
	}

	// If the operator CRD is installed, fetch CRs so we can mark each cluster
	// as legacy- or operator-managed and surface CRs that have not yet produced
	// a backing namespace.
	crsByName := map[string]k8s.VClusterCR{}
	if available, _ := k8s.IsOperatorAvailable(ctx, restConfig); available {
		crs, err := k8s.ListVirtualClusterCRs(ctx, restConfig)
		if err != nil {
			return fmt.Errorf("listing VirtualCluster CRs: %w", err)
		}
		for _, cr := range crs {
			crsByName[cr.Name] = cr
		}
	}

	type row struct {
		name      string
		namespace string
		status    string
		mode      string
		created   string
	}
	var rows []row

	seen := map[string]bool{}
	for _, c := range clusters {
		mode := "legacy"
		if _, ok := crsByName[c.Name]; ok {
			mode = "operator"
		}
		rows = append(rows, row{c.Name, c.Namespace, c.Status, mode, c.Created})
		seen[c.Name] = true
	}

	// Add CRs that don't yet have a host namespace (operator hasn't reconciled them).
	for name, cr := range crsByName {
		if seen[name] {
			continue
		}
		status := cr.Phase
		if status == "" {
			status = "Pending"
		}
		rows = append(rows, row{cr.Name, cr.Namespace + " (CR)", status, "operator", ""})
	}

	if len(rows) == 0 {
		fmt.Println("No virtual clusters found.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "NAME\tNAMESPACE\tSTATUS\tMODE\tCREATED")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.name, r.namespace, r.status, r.mode, r.created)
	}
	return w.Flush()
}
