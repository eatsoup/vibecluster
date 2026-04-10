package cli

import (
	"fmt"
	"os"
	"time"

	"github.com/eatsoup/vibecluster/pkg/k8s"
	"github.com/eatsoup/vibecluster/pkg/kubeconfig"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type exposeOptions struct {
	exposeType    string
	ingressClass  string
	host          string
	temp          bool
	kubeconfigOut string
}

func newExposeCommand() *cobra.Command {
	opts := &exposeOptions{}

	cmd := &cobra.Command{
		Use:   "expose NAME",
		Short: "Expose a virtual cluster via LoadBalancer, Ingress, or an ephemeral port-forward",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runExpose(args[0], opts)
		},
	}

	cmd.Flags().StringVar(&opts.exposeType, "type", "", "exposure type for the cluster (LoadBalancer, Ingress)")
	cmd.Flags().StringVar(&opts.ingressClass, "ingress-class", "", "ingress class if expose is Ingress")
	cmd.Flags().StringVar(&opts.host, "host", "", "ingress hostname if expose is Ingress")
	cmd.Flags().BoolVar(&opts.temp, "temp", false, "start an ephemeral port-forward to the virtual cluster API and block until Ctrl+C")
	cmd.Flags().StringVar(&opts.kubeconfigOut, "kubeconfig", "", "write a kubeconfig pointing at the new external address to this file (default: ./vibecluster-<name>.kubeconfig)")

	return cmd
}

func runExpose(name string, opts *exposeOptions) error {
	if opts.temp && opts.exposeType != "" {
		return fmt.Errorf("--temp and --type are mutually exclusive")
	}
	if !opts.temp && opts.exposeType == "" {
		return fmt.Errorf("either --type or --temp must be specified")
	}
	if opts.temp {
		return runExposeTemp(name, opts)
	}
	return runExposePersistent(name, opts)
}

func runExposeTemp(name string, opts *exposeOptions) error {
	client, restConfig, err := k8s.NewClient(kubeContext)
	if err != nil {
		return err
	}

	ctx := cmd_context()
	ns := k8s.NamespaceName(name)
	podName := name + "-0"

	fmt.Fprintf(os.Stderr, "Setting up port-forward to %s/%s...\n", ns, podName)
	localPort, stopCh, err := k8s.PortForward(ctx, client, restConfig, ns, podName, k8s.K3sPort)
	if err != nil {
		return fmt.Errorf("port-forward: %w", err)
	}
	defer close(stopCh)

	server := fmt.Sprintf("https://127.0.0.1:%d", localPort)

	fmt.Fprintln(os.Stderr, "Retrieving kubeconfig...")
	config, err := kubeconfig.Retrieve(ctx, client, restConfig, name, server)
	if err != nil {
		return fmt.Errorf("retrieving kubeconfig: %w", err)
	}

	outPath := opts.kubeconfigOut
	if outPath == "" {
		outPath = fmt.Sprintf("./vibecluster-%s.kubeconfig", name)
	}
	if err := kubeconfig.WriteToFile(config, outPath); err != nil {
		return fmt.Errorf("writing kubeconfig: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Kubeconfig written to %s\n", outPath)
	fmt.Fprintf(os.Stderr, "Port-forward running on 127.0.0.1:%d. Press Ctrl+C to stop.\n", localPort)
	fmt.Fprintf(os.Stderr, "  export KUBECONFIG=%s\n", outPath)

	<-ctx.Done()
	fmt.Fprintln(os.Stderr, "Stopping port-forward.")
	return nil
}

func runExposePersistent(name string, opts *exposeOptions) error {
	client, restConfig, err := k8s.NewClient(kubeContext)
	if err != nil {
		return err
	}

	ctx := cmd_context()
	ns := k8s.NamespaceName(name)
	labels := k8s.Labels(name)
	_ = labels

	if opts.exposeType != "LoadBalancer" && opts.exposeType != "Ingress" {
		return fmt.Errorf("unsupported expose type %q, must be LoadBalancer or Ingress", opts.exposeType)
	}

	createOpts := k8s.CreateOptions{
		ExposeType:         opts.exposeType,
		ExposeIngressClass: opts.ingressClass,
		ExposeHost:         opts.host,
	}
	_ = createOpts // Using inline since manifest.go was changed to accept CreateOptions in builders or directly

	fmt.Printf("Exposing virtual cluster %q via %s...\n", name, opts.exposeType)

	// Update the service to LoadBalancer if requested
	svc, err := client.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting service: %w", err)
	}

	needsUpdate := false
	if opts.exposeType == "LoadBalancer" && svc.Spec.Type != "LoadBalancer" {
		svc.Spec.Type = "LoadBalancer"
		needsUpdate = true
	} else if opts.exposeType == "Ingress" && svc.Spec.Type != "ClusterIP" {
		svc.Spec.Type = "ClusterIP"
		needsUpdate = true
	}

	if needsUpdate {
		if _, err := client.CoreV1().Services(ns).Update(ctx, svc, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("updating service: %w", err)
		}
		fmt.Printf("  Updated Service %q type to %s\n", name, svc.Spec.Type)
	}

	// For Ingress, we wait on manifests.go to be updated which we will do next.
	// Since builders.go doesn't exist on main, we will manually build Ingress here via k8s.BuildIngress (wait, we need to add BuildIngress to manifests.go or here).
	// Actually, let me just add BuildIngress to manifests.go so it works as it did.
	
	if opts.exposeType == "Ingress" {
		ing := k8s.BuildIngress(name, ns, labels, opts.host, opts.ingressClass)
		existing, err := client.NetworkingV1().Ingresses(ns).Get(ctx, name, metav1.GetOptions{})
		if errors.IsNotFound(err) {
			_, err = client.NetworkingV1().Ingresses(ns).Create(ctx, ing, metav1.CreateOptions{})
			if err != nil {
				return fmt.Errorf("creating ingress: %w", err)
			}
			fmt.Printf("  Created Ingress %q\n", name)
		} else if err == nil {
			existing.Spec = ing.Spec
			if existing.Annotations == nil {
				existing.Annotations = make(map[string]string)
			}
			for k, v := range ing.Annotations {
				existing.Annotations[k] = v
			}
			_, err = client.NetworkingV1().Ingresses(ns).Update(ctx, existing, metav1.UpdateOptions{})
			if err != nil {
				return fmt.Errorf("updating ingress: %w", err)
			}
			fmt.Printf("  Updated Ingress %q\n", name)
		}

		// Update StatefulSet TLS-SAN
		if opts.host != "" {
			sts, err := client.AppsV1().StatefulSets(ns).Get(ctx, name, metav1.GetOptions{})
			if err == nil {
				for i, c := range sts.Spec.Template.Spec.Containers {
					if c.Name == "k3s" {
						hasSan := false
						sanArg := "--tls-san=" + opts.host
						for _, arg := range c.Args {
							if arg == sanArg {
								hasSan = true
								break
							}
						}
						if !hasSan {
							sts.Spec.Template.Spec.Containers[i].Args = append(c.Args, sanArg)
							_, err = client.AppsV1().StatefulSets(ns).Update(ctx, sts, metav1.UpdateOptions{})
							if err != nil {
								return fmt.Errorf("updating statefulset TLS SAN: %w", err)
							}
							fmt.Printf("  Updated StatefulSet K3s args with --tls-san=%s\n", opts.host)
						}
						break
					}
				}
			}
		}
	} else {
		err := client.NetworkingV1().Ingresses(ns).Delete(ctx, name, metav1.DeleteOptions{})
		if err == nil {
			fmt.Printf("  Deleted Ingress %q\n", name)
		}
	}

	fmt.Println("Expose configuration applied successfully.")

	// Wait for the external address to materialise (LB IP, or just confirm Ingress host),
	// then write a fresh kubeconfig pointing at it so users can connect immediately.
	fmt.Println("Waiting for external address...")
	addr, err := k8s.WaitForExternalAddress(ctx, client, name, 5*time.Minute)
	if err != nil {
		return fmt.Errorf("waiting for external address: %w", err)
	}
	fmt.Printf("External address: %s\n", addr.URL())

	fmt.Println("Retrieving kubeconfig...")
	cfg, err := kubeconfig.RetrieveWithOptions(ctx, client, restConfig, name, kubeconfig.RetrieveOptions{
		Server:                addr.URL(),
		InsecureSkipTLSVerify: !addr.CertVerifies,
	})
	if err != nil {
		return fmt.Errorf("retrieving kubeconfig: %w", err)
	}

	outPath := opts.kubeconfigOut
	if outPath == "" {
		outPath = fmt.Sprintf("./vibecluster-%s.kubeconfig", name)
	}
	if err := kubeconfig.WriteToFile(cfg, outPath); err != nil {
		return fmt.Errorf("writing kubeconfig: %w", err)
	}
	fmt.Printf("Kubeconfig written to %s\n", outPath)
	if !addr.CertVerifies {
		fmt.Println("Note: kubeconfig uses insecure-skip-tls-verify because the LoadBalancer address is not in the k3s server certificate SANs.")
	}
	return nil
}
