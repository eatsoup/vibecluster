// Package helpers provides shared utilities for the vibecluster e2e test suite.
package helpers

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/tools/clientcmd"
)

// Globals set by TestMain before any test runs.
var (
	VibeclusterBin  string // path to the vibecluster binary
	HostKubeconfig  string // kubeconfig for the k3d host cluster
	SyncerImage     string // syncer image to use when creating vclusters
	OperatorImage   string // operator image to use when installing the operator
)

// UniqueName returns a short unique name safe for use as a Kubernetes resource name.
func UniqueName(prefix string) string {
	return fmt.Sprintf("%s-%05d", prefix, rand.Intn(100000))
}

// RunVibeCluster executes the vibecluster CLI with the given arguments against
// the host cluster and returns stdout. On error it returns the combined output
// so callers can inspect it.
func RunVibeCluster(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(VibeclusterBin, args...)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+HostKubeconfig)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	combined := stdout.String() + stderr.String()
	if err != nil {
		return combined, fmt.Errorf("vibecluster %s: %w\n%s", strings.Join(args, " "), err, combined)
	}
	return combined, nil
}

// MustVibeCluster is like RunVibeCluster but calls t.Fatal on error.
func MustVibeCluster(t *testing.T, args ...string) string {
	t.Helper()
	out, err := RunVibeCluster(t, args...)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return out
}

// Kubectl runs kubectl with the given kubeconfig and returns stdout.
func Kubectl(t *testing.T, kubeconfig string, args ...string) (string, error) {
	t.Helper()
	fullArgs := append([]string{"--kubeconfig", kubeconfig}, args...)
	cmd := exec.Command("kubectl", fullArgs...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("kubectl %s: %w\nstdout: %s\nstderr: %s",
			strings.Join(args, " "), err, stdout.String(), stderr.String())
	}
	return stdout.String(), nil
}

// MustKubectl is like Kubectl but calls t.Fatal on error.
func MustKubectl(t *testing.T, kubeconfig string, args ...string) string {
	t.Helper()
	out, err := Kubectl(t, kubeconfig, args...)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return out
}

// KubectlExec runs a command inside a pod container and returns stdout.
func KubectlExec(t *testing.T, kubeconfig, ns, pod, container string, cmd ...string) (string, error) {
	t.Helper()
	args := []string{"exec", pod, "-n", ns, "-c", container, "--"}
	args = append(args, cmd...)
	return Kubectl(t, kubeconfig, args...)
}

// MustKubectlExec is like KubectlExec but calls t.Fatal on error.
func MustKubectlExec(t *testing.T, kubeconfig, ns, pod, container string, cmd ...string) string {
	t.Helper()
	out, err := KubectlExec(t, kubeconfig, ns, pod, container, cmd...)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return out
}

// WaitFor polls condition until it returns nil or timeout is exceeded.
func WaitFor(t *testing.T, timeout, interval time.Duration, condition func() error) error {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		lastErr = condition()
		if lastErr == nil {
			return nil
		}
		time.Sleep(interval)
	}
	return fmt.Errorf("timed out after %s: %v", timeout, lastErr)
}

// MustWaitFor is like WaitFor but calls t.Fatal on timeout.
func MustWaitFor(t *testing.T, timeout, interval time.Duration, condition func() error) {
	t.Helper()
	if err := WaitFor(t, timeout, interval, condition); err != nil {
		DumpDebugAll(t)
		t.Fatalf("WaitFor: %v", err)
	}
}

// WaitForPodRunning waits until at least one pod matching labelSelector in ns
// is in the Running phase.
func WaitForPodRunning(t *testing.T, kubeconfig, ns, labelSelector string, timeout time.Duration) {
	t.Helper()
	MustWaitFor(t, timeout, 5*time.Second, func() error {
		out, err := Kubectl(t, kubeconfig, "get", "pods", "-n", ns,
			"-l", labelSelector, "-o", "jsonpath={.items[*].status.phase}")
		if err != nil {
			return err
		}
		for _, phase := range strings.Fields(out) {
			if phase == "Running" {
				return nil
			}
		}
		return fmt.Errorf("no Running pods in %s matching %s (got %q)", ns, labelSelector, out)
	})
}

// WaitForStatefulSetReady waits until all replicas of the StatefulSet are ready.
func WaitForStatefulSetReady(t *testing.T, kubeconfig, ns, name string, timeout time.Duration) {
	t.Helper()
	MustWaitFor(t, timeout, 5*time.Second, func() error {
		out, err := Kubectl(t, kubeconfig, "get", "statefulset", name, "-n", ns,
			"-o", "jsonpath={.status.readyReplicas}")
		if err != nil {
			return err
		}
		if strings.TrimSpace(out) == "0" || strings.TrimSpace(out) == "" {
			return fmt.Errorf("statefulset %s/%s not ready yet", ns, name)
		}
		return nil
	})
}

// StartPortForward starts a kubectl port-forward from a random local port to
// remotePort on the given pod, and returns the local port plus a stop function.
// The port-forward is automatically stopped when t completes.
func StartPortForward(t *testing.T, ns, pod string, remotePort int) (int, func()) {
	t.Helper()

	localPort := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())

	cmd := exec.CommandContext(ctx, "kubectl",
		"--kubeconfig", HostKubeconfig,
		"port-forward",
		"-n", ns,
		"pod/"+pod,
		fmt.Sprintf("%d:%d", localPort, remotePort),
	)
	cmd.Stdout = os.Stderr // route pf logs to stderr so -v shows them
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("starting port-forward %s/%s:%d: %v", ns, pod, remotePort, err)
	}

	// Wait for the local port to accept connections (up to 30 s).
	addr := fmt.Sprintf("127.0.0.1:%d", localPort)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			conn, err := net.DialTimeout("tcp", addr, time.Second)
			if err == nil {
				conn.Close()
				return
			}
			time.Sleep(500 * time.Millisecond)
		}
	}()
	wg.Wait()

	stop := func() {
		cancel()
		cmd.Wait() //nolint:errcheck
	}
	t.Cleanup(stop)
	return localPort, stop
}

// GetVClusterKubeconfig sets up a port-forward to the named virtual cluster's
// k3s API server, extracts TLS credentials, and returns the path to a
// kubeconfig file valid for the duration of the test.
//
// It assumes the StatefulSet pod NAME-0 is already Running in namespace vc-NAME.
func GetVClusterKubeconfig(t *testing.T, name string) string {
	t.Helper()
	ns := "vc-" + name
	pod := name + "-0"

	// Ensure the pod is Running first.
	WaitForStatefulSetReady(t, HostKubeconfig, ns, name, 5*time.Minute)

	// Port-forward to the k3s API server port inside the pod.
	localPort, _ := StartPortForward(t, ns, pod, 6443)

	// Extract TLS credentials via exec.
	caData := []byte(strings.TrimSpace(MustKubectlExec(t, HostKubeconfig, ns, pod, "k3s",
		"cat", "/data/k3s/server/tls/server-ca.crt")))
	clientCert := []byte(strings.TrimSpace(MustKubectlExec(t, HostKubeconfig, ns, pod, "k3s",
		"cat", "/data/k3s/server/tls/client-admin.crt")))
	clientKey := []byte(strings.TrimSpace(MustKubectlExec(t, HostKubeconfig, ns, pod, "k3s",
		"cat", "/data/k3s/server/tls/client-admin.key")))

	server := fmt.Sprintf("https://127.0.0.1:%d", localPort)
	kcPath := filepath.Join(t.TempDir(), name+".kubeconfig")
	writeKubeconfig(t, kcPath, name, server, caData, clientCert, clientKey)

	// Smoke-test: wait until the virtual API is reachable.
	MustWaitFor(t, 60*time.Second, 3*time.Second, func() error {
		_, err := Kubectl(t, kcPath, "get", "nodes")
		return err
	})

	return kcPath
}

// writeKubeconfig writes a kubeconfig that uses insecure-skip-tls-verify so
// port-forwarded connections (where 127.0.0.1 is not in the k3s cert SAN) work.
func writeKubeconfig(t *testing.T, path, name, server string, _, clientCert, clientKey []byte) {
	t.Helper()
	clusterKey := "vibe-" + name
	cfg := &clientcmdapi.Config{
		Clusters: map[string]*clientcmdapi.Cluster{
			clusterKey: {
				Server:                server,
				InsecureSkipTLSVerify: true,
			},
		},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{
			clusterKey: {
				ClientCertificateData: clientCert,
				ClientKeyData:         clientKey,
			},
		},
		Contexts: map[string]*clientcmdapi.Context{
			clusterKey: {Cluster: clusterKey, AuthInfo: clusterKey},
		},
		CurrentContext: clusterKey,
	}
	if err := clientcmd.WriteToFile(*cfg, path); err != nil {
		t.Fatalf("writing vcluster kubeconfig: %v", err)
	}
}

// CreateVCluster creates a virtual cluster, registers cleanup via t.Cleanup,
// and returns the path to a kubeconfig that can reach its API server.
//
// extraArgs are appended to `vibecluster create NAME --mode=legacy --connect=false`.
func CreateVCluster(t *testing.T, name string, extraArgs ...string) string {
	t.Helper()
	args := append([]string{"create", name, "--mode=legacy", "--connect=false"}, extraArgs...)
	if SyncerImage != "" {
		args = append(args, "--syncer-image", SyncerImage)
	}
	MustVibeCluster(t, args...)

	t.Cleanup(func() {
		// Best-effort cleanup; ignore errors (test may have already deleted it).
		RunVibeCluster(t, "delete", name) //nolint:errcheck
	})

	return GetVClusterKubeconfig(t, name)
}

// DumpDebug logs kubectl output for the vc-NAME namespace on test failure.
func DumpDebug(t *testing.T, ns string) {
	t.Helper()
	if !t.Failed() {
		return
	}
	for _, args := range [][]string{
		{"get", "all", "-n", ns},
		{"describe", "pods", "-n", ns},
		{"get", "events", "-n", ns, "--sort-by=.lastTimestamp"},
	} {
		out, err := Kubectl(t, HostKubeconfig, args...)
		if err == nil {
			t.Logf("=== kubectl %s ===\n%s", strings.Join(args, " "), out)
		}
	}
}

// DumpDebugAll logs kubectl output across all vc-* namespaces on test failure.
func DumpDebugAll(t *testing.T) {
	t.Helper()
	if !t.Failed() {
		return
	}
	out, err := Kubectl(t, HostKubeconfig, "get", "namespaces", "-o",
		"jsonpath={.items[*].metadata.name}")
	if err != nil {
		return
	}
	for _, ns := range strings.Fields(out) {
		if strings.HasPrefix(ns, "vc-") {
			DumpDebug(t, ns)
		}
	}
}

// MustKubectlApply applies a YAML manifest string using kubectl apply -f -.
func MustKubectlApply(t *testing.T, kubeconfig, yaml string) {
	t.Helper()
	cmd := exec.Command("kubectl", "--kubeconfig", kubeconfig, "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(yaml)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("kubectl apply: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}
}

// ErrNotFound returns a formatted error indicating something was not found in output.
func ErrNotFound(needle, haystack string) error {
	return fmt.Errorf("%q not found in output:\n%s", needle, haystack)
}

// freePort asks the OS for an available TCP port.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("finding free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}
