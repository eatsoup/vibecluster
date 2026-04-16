package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/eatsoup/vibecluster/pkg/k8s"
	"github.com/eatsoup/vibecluster/pkg/syncer"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	name := os.Getenv("VCLUSTER_NAME")
	if name == "" {
		fmt.Fprintln(os.Stderr, "VCLUSTER_NAME environment variable is required")
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-ch
		fmt.Println("Shutting down syncer...")
		cancel()
	}()

	// In vnode mode there is a real in-vcluster
	// kubelet, so the flat workload syncer and the kubelet shim are both
	// unnecessary. Before idling, seed the coredns NodeHosts key so the
	// in-vcluster coredns can start: k3s with --disable-agent does not
	// create that key on its own, and coredns stays in ContainerCreating
	// with "configmap references non-existent config key: NodeHosts".
	// Once the key exists, k3s's own NodeHosts controller updates it as
	// nodes register.
	if os.Getenv(k8s.EnvVNodeMode) == "true" {
		fmt.Println("VIBE_VNODE_MODE=true: workload sync and kubelet shim disabled.")
		if err := bootstrapVNode(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "vnode bootstrap: %v\n", err)
		}
		fmt.Println("vnode bootstrap complete; idling.")
		<-ctx.Done()
		return
	}

	// Wait for k3s to be ready (kubeconfig file to appear)
	vcKubeconfig := "/data/k3s/server/cred/admin.kubeconfig"
	fmt.Printf("Waiting for k3s kubeconfig at %s...\n", vcKubeconfig)
	if err := waitForFile(ctx, vcKubeconfig, 5*time.Minute); err != nil {
		fmt.Fprintf(os.Stderr, "Error waiting for k3s: %v\n", err)
		os.Exit(1)
	}

	// Give k3s a moment to fully initialize after writing creds
	time.Sleep(5 * time.Second)

	// Build virtual cluster client from k3s kubeconfig
	vcConfig, err := clientcmd.BuildConfigFromFlags("", vcKubeconfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading virtual cluster kubeconfig: %v\n", err)
		os.Exit(1)
	}

	vClient, err := kubernetes.NewForConfig(vcConfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating virtual cluster client: %v\n", err)
		os.Exit(1)
	}

	// Build host cluster client from in-cluster service account
	hostConfig, err := rest.InClusterConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading host cluster config: %v\n", err)
		os.Exit(1)
	}

	hostClient, err := kubernetes.NewForConfig(hostConfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating host cluster client: %v\n", err)
		os.Exit(1)
	}

	// Wait for k3s API to actually be responding
	fmt.Println("Waiting for virtual cluster API to be ready...")
	if err := waitForAPI(ctx, vClient, 3*time.Minute); err != nil {
		fmt.Fprintf(os.Stderr, "Error waiting for virtual cluster API: %v\n", err)
		os.Exit(1)
	}

	// Start syncing
	s := syncer.New(name, hostClient, vClient)

	// Configure the kubelet shim. POD_IP is supplied via the downward API
	// in the StatefulSet template; we use it both as the bind / SAN IP for
	// the shim's serving cert and as the InternalIP we patch onto every
	// synced virtual node so the virtual k3s API server forwards
	// logs/exec/portforward requests to us instead of the real host
	// kubelet. The k3s data dir is mounted into the syncer container so
	// the shim can sign its serving cert with the k3s server CA.
	s.ShimHostConfig = hostConfig
	s.ShimPodIP = os.Getenv("POD_IP")
	s.ShimPort = k8s.KubeletShimPort
	s.ShimCACertPath = "/data/k3s/server/tls/server-ca.crt"
	s.ShimCAKeyPath = "/data/k3s/server/tls/server-ca.key"
	if s.ShimPodIP == "" {
		fmt.Fprintln(os.Stderr, "warning: POD_IP env var is empty; kubelet shim will not be reachable from the virtual k3s API server")
	}

	if err := s.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Syncer error: %v\n", err)
		os.Exit(1)
	}
}

func waitForFile(ctx context.Context, path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("timeout waiting for %s", path)
}

// bootstrapVNode is the vnode-mode one-shot bootstrap: connect to the
// in-vcluster k3s API as the cluster admin (via the kubeconfig k3s writes
// to the shared data volume) and make sure the coredns ConfigMap has a
// NodeHosts key. Errors are logged and swallowed — the vcluster stays
// usable without coredns, and retrying on pod restart is fine.
func bootstrapVNode(ctx context.Context) error {
	vcKubeconfig := "/data/k3s/server/cred/admin.kubeconfig"
	if err := waitForFile(ctx, vcKubeconfig, 5*time.Minute); err != nil {
		return fmt.Errorf("waiting for kubeconfig: %w", err)
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", vcKubeconfig)
	if err != nil {
		return fmt.Errorf("loading kubeconfig: %w", err)
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("building client: %w", err)
	}
	return seedCoreDNSNodeHosts(ctx, client)
}

// seedCoreDNSNodeHosts polls for the kube-system/coredns ConfigMap and adds
// an empty NodeHosts key if missing. k3s's NodeHosts controller takes over
// from there and fills in real entries as nodes register.
func seedCoreDNSNodeHosts(ctx context.Context, client kubernetes.Interface) error {
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		cm, err := client.CoreV1().ConfigMaps("kube-system").Get(ctx, "coredns", metav1.GetOptions{})
		if err == nil {
			if _, ok := cm.Data["NodeHosts"]; ok {
				return nil
			}
			patch := []byte(`{"data":{"NodeHosts":""}}`)
			_, err = client.CoreV1().ConfigMaps("kube-system").Patch(ctx, "coredns", types.StrategicMergePatchType, patch, metav1.PatchOptions{})
			if err != nil {
				return fmt.Errorf("patching coredns configmap: %w", err)
			}
			fmt.Println("coredns NodeHosts seeded.")
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("timeout waiting for kube-system/coredns configmap")
}

func waitForAPI(ctx context.Context, client *kubernetes.Clientset, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, err := client.Discovery().ServerVersion()
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("timeout waiting for API server")
}
