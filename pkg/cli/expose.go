package cli

import (
	"fmt"

	"github.com/eatsoup/vibecluster/pkg/k8s"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type exposeOptions struct {
	exposeType   string
	ingressClass string
	host         string
}

func newExposeCommand() *cobra.Command {
	opts := &exposeOptions{}

	cmd := &cobra.Command{
		Use:   "expose NAME",
		Short: "Expose a virtual cluster via LoadBalancer or Ingress",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runExpose(args[0], opts)
		},
	}

	cmd.Flags().StringVar(&opts.exposeType, "type", "", "exposure type for the cluster (LoadBalancer, Ingress)")
	cmd.Flags().StringVar(&opts.ingressClass, "ingress-class", "", "ingress class if expose is Ingress")
	cmd.Flags().StringVar(&opts.host, "host", "", "ingress hostname if expose is Ingress")
	_ = cmd.MarkFlagRequired("type")

	return cmd
}

func runExpose(name string, opts *exposeOptions) error {
	client, _, err := k8s.NewClient(kubeContext)
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
	return nil
}
