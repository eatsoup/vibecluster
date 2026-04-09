package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/eatsoup/vibecluster/pkg/k8s"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type logsOptions struct {
	follow    bool
	container string
}

func newLogsCommand() *cobra.Command {
	opts := &logsOptions{}

	cmd := &cobra.Command{
		Use:   "logs NAME",
		Short: "View logs from the virtual cluster (syncer or k3s)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLogs(args[0], opts)
		},
	}

	cmd.Flags().BoolVarP(&opts.follow, "follow", "f", false, "follow log output")
	cmd.Flags().StringVarP(&opts.container, "container", "c", "syncer", "container to get logs from (syncer or k3s)")

	return cmd
}

func runLogs(name string, opts *logsOptions) error {
	client, _, err := k8s.NewClient(kubeContext)
	if err != nil {
		return err
	}

	ctx := cmd_context()
	ns := k8s.NamespaceName(name)
	podName := name + "-0"

	logOpts := &corev1.PodLogOptions{
		Container: opts.container,
		Follow:    opts.follow,
	}

	req := client.CoreV1().Pods(ns).GetLogs(podName, logOpts)
	stream, err := req.Stream(ctx)
	if err != nil {
		// Check if the pod exists first
		_, podErr := client.CoreV1().Pods(ns).Get(ctx, podName, metav1.GetOptions{})
		if podErr != nil {
			return fmt.Errorf("virtual cluster %q not found or not running", name)
		}
		return fmt.Errorf("getting logs: %w", err)
	}
	defer stream.Close()

	_, err = io.Copy(os.Stdout, stream)
	return err
}
