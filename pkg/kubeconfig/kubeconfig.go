package kubeconfig

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/eatsoup/vibecluster/pkg/k8s"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/tools/remotecommand"
)

// Retrieve fetches the kubeconfig from the k3s pod and rewrites the server URL.
func Retrieve(ctx context.Context, client *kubernetes.Clientset, restConfig *rest.Config, name string, serverOverride string) (*clientcmdapi.Config, error) {
	ns := k8s.NamespaceName(name)
	podName := name + "-0" // StatefulSet pod naming

	// Read cert files directly from the k3s pod
	caData, err := execInPod(ctx, client, restConfig, ns, podName, "k3s",
		[]string{"cat", "/data/k3s/server/tls/server-ca.crt"})
	if err != nil {
		return nil, fmt.Errorf("reading CA cert from pod: %w", err)
	}

	clientCert, err := execInPod(ctx, client, restConfig, ns, podName, "k3s",
		[]string{"cat", "/data/k3s/server/tls/client-admin.crt"})
	if err != nil {
		return nil, fmt.Errorf("reading client cert from pod: %w", err)
	}

	clientKey, err := execInPod(ctx, client, restConfig, ns, podName, "k3s",
		[]string{"cat", "/data/k3s/server/tls/client-admin.key"})
	if err != nil {
		return nil, fmt.Errorf("reading client key from pod: %w", err)
	}

	// Determine server address
	server := serverOverride
	if server == "" {
		server = fmt.Sprintf("https://%s.%s.svc.cluster.local:%d", name, ns, k8s.ServicePort)
	}

	// Build kubeconfig with embedded certs
	newConfig := clientcmdapi.NewConfig()

	clusterName := "vibecluster-" + name
	userName := "vibecluster-" + name
	contextName := "vibecluster-" + name

	newConfig.Clusters[clusterName] = &clientcmdapi.Cluster{
		Server:                   server,
		CertificateAuthorityData: caData,
	}

	newConfig.AuthInfos[userName] = &clientcmdapi.AuthInfo{
		ClientCertificateData: clientCert,
		ClientKeyData:         clientKey,
	}

	newConfig.Contexts[contextName] = &clientcmdapi.Context{
		Cluster:  clusterName,
		AuthInfo: userName,
	}
	newConfig.CurrentContext = contextName

	return newConfig, nil
}

// WriteToFile writes the kubeconfig to the default kubeconfig location,
// merging with existing config.
func WriteToFile(config *clientcmdapi.Config, outputPath string) error {
	if outputPath == "" {
		outputPath = defaultKubeconfigPath()
	}

	// If the file exists, merge
	existing, err := clientcmd.LoadFromFile(outputPath)
	if err != nil {
		// File doesn't exist or is invalid, just write new
		existing = clientcmdapi.NewConfig()
	}

	// Merge clusters, users, contexts
	for k, v := range config.Clusters {
		existing.Clusters[k] = v
	}
	for k, v := range config.AuthInfos {
		existing.AuthInfos[k] = v
	}
	for k, v := range config.Contexts {
		existing.Contexts[k] = v
	}
	existing.CurrentContext = config.CurrentContext

	// Ensure directory exists
	dir := filepath.Dir(outputPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("creating kubeconfig directory: %w", err)
	}

	return clientcmd.WriteToFile(*existing, outputPath)
}

// Print writes the kubeconfig to stdout.
func Print(config *clientcmdapi.Config) error {
	data, err := clientcmd.Write(*config)
	if err != nil {
		return err
	}
	fmt.Print(string(data))
	return nil
}

// RemoveFromFile removes the virtual cluster from the kubeconfig file.
func RemoveFromFile(name string, outputPath string) error {
	if outputPath == "" {
		outputPath = defaultKubeconfigPath()
	}

	config, err := clientcmd.LoadFromFile(outputPath)
	if err != nil {
		return nil // nothing to remove
	}

	contextName := "vibecluster-" + name
	clusterName := "vibecluster-" + name
	userName := "vibecluster-" + name

	delete(config.Contexts, contextName)
	delete(config.Clusters, clusterName)
	delete(config.AuthInfos, userName)

	if config.CurrentContext == contextName {
		config.CurrentContext = ""
	}

	return clientcmd.WriteToFile(*config, outputPath)
}

func defaultKubeconfigPath() string {
	if env := os.Getenv("KUBECONFIG"); env != "" {
		// Use first path if multiple
		parts := strings.SplitN(env, ":", 2)
		return parts[0]
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".kube", "config")
}

func execInPod(ctx context.Context, client *kubernetes.Clientset, restConfig *rest.Config, namespace, podName, container string, command []string) ([]byte, error) {
	req := client.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   command,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(restConfig, "POST", req.URL())
	if err != nil {
		return nil, fmt.Errorf("creating executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		return nil, fmt.Errorf("exec failed: %w (stderr: %s)", err, stderr.String())
	}

	return stdout.Bytes(), nil
}
