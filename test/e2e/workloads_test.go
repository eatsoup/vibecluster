//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/eatsoup/vibecluster/test/e2e/helpers"
)

// TestNginxDeployment creates a virtual cluster, deploys nginx, waits for the
// pod to reach Running, and confirms the syncer has mirrored it into the host
// namespace under the translated name.
func TestNginxDeployment(t *testing.T) {
	name := helpers.UniqueName("wl")
	ns := "vc-" + name
	defer helpers.DumpDebug(t, ns)

	vcKubeconfig := helpers.CreateVCluster(t, name)

	// Deploy nginx into the virtual cluster.
	helpers.MustKubectl(t, vcKubeconfig,
		"create", "deployment", "nginx", "--image=nginx:alpine", "--replicas=1")

	// Wait for the deployment's pod to be Running inside the virtual cluster.
	helpers.WaitForPodRunning(t, vcKubeconfig, "default", "app=nginx", 5*time.Minute)

	// The syncer translates virtual pod names to the pattern:
	//   <clustername>-x-<podname>-x-<namespace>
	// Verify at least one pod with the expected prefix exists in the host namespace.
	helpers.MustWaitFor(t, 2*time.Minute, 5*time.Second, func() error {
		out, err := helpers.Kubectl(t, helpers.HostKubeconfig,
			"get", "pods", "-n", ns, "-o", "jsonpath={.items[*].metadata.name}")
		if err != nil {
			return err
		}
		prefix := name + "-x-nginx"
		for _, podName := range strings.Fields(out) {
			if strings.HasPrefix(podName, prefix) {
				return nil
			}
		}
		return fmt.Errorf("no host pod with prefix %q found; got: %s", prefix, out)
	})
}

// TestPodToPodCommunication deploys two services inside a virtual cluster and
// verifies that a curl from one pod reaches the other via ClusterIP service.
func TestPodToPodCommunication(t *testing.T) {
	name := helpers.UniqueName("p2p")
	ns := "vc-" + name
	defer helpers.DumpDebug(t, ns)

	vcKubeconfig := helpers.CreateVCluster(t, name)

	// Deploy a simple HTTP server (nginx) and expose it.
	helpers.MustKubectl(t, vcKubeconfig,
		"create", "deployment", "server", "--image=nginx:alpine")
	helpers.MustKubectl(t, vcKubeconfig,
		"expose", "deployment", "server", "--port=80", "--name=server-svc")

	// Deploy a curl client pod.
	helpers.MustKubectl(t, vcKubeconfig,
		"run", "client", "--image=curlimages/curl:latest",
		"--restart=Never",
		"--command", "--", "sleep", "3600")

	// Wait for both pods to be Running.
	helpers.WaitForPodRunning(t, vcKubeconfig, "default", "app=server", 5*time.Minute)
	helpers.MustWaitFor(t, 3*time.Minute, 5*time.Second, func() error {
		out, err := helpers.Kubectl(t, vcKubeconfig,
			"get", "pod", "client", "-o", "jsonpath={.status.phase}")
		if err != nil {
			return err
		}
		if strings.TrimSpace(out) != "Running" {
			return fmt.Errorf("client pod phase: %s", out)
		}
		return nil
	})

	// curl from the client pod to the server's ClusterIP service.
	helpers.MustWaitFor(t, 60*time.Second, 5*time.Second, func() error {
		out, err := helpers.KubectlExec(t, vcKubeconfig,
			"default", "client", "client",
			"curl", "-sf", "--max-time", "5", "http://server-svc/")
		if err != nil {
			return fmt.Errorf("curl failed: %v", err)
		}
		if !strings.Contains(out, "Welcome to nginx") {
			return fmt.Errorf("unexpected response: %s", out)
		}
		return nil
	})
}

// TestDNSResolution verifies that a pod inside the virtual cluster can resolve
// another service by its full DNS name (<svc>.<ns>.svc.cluster.local).
func TestDNSResolution(t *testing.T) {
	name := helpers.UniqueName("dns")
	ns := "vc-" + name
	defer helpers.DumpDebug(t, ns)

	vcKubeconfig := helpers.CreateVCluster(t, name)

	helpers.MustKubectl(t, vcKubeconfig,
		"create", "deployment", "web", "--image=nginx:alpine")
	helpers.MustKubectl(t, vcKubeconfig,
		"expose", "deployment", "web", "--port=80", "--name=web-svc")
	helpers.MustKubectl(t, vcKubeconfig,
		"run", "resolver", "--image=curlimages/curl:latest",
		"--restart=Never",
		"--command", "--", "sleep", "3600")

	helpers.WaitForPodRunning(t, vcKubeconfig, "default", "app=web", 5*time.Minute)
	helpers.MustWaitFor(t, 3*time.Minute, 5*time.Second, func() error {
		out, _ := helpers.Kubectl(t, vcKubeconfig,
			"get", "pod", "resolver", "-o", "jsonpath={.status.phase}")
		if strings.TrimSpace(out) == "Running" {
			return nil
		}
		return fmt.Errorf("resolver pod phase: %s", out)
	})

	// Use the full FQDN.
	fqdn := "web-svc.default.svc.cluster.local"
	helpers.MustWaitFor(t, 60*time.Second, 5*time.Second, func() error {
		_, err := helpers.KubectlExec(t, vcKubeconfig,
			"default", "resolver", "resolver",
			"curl", "-sf", "--max-time", "5", "http://"+fqdn+"/")
		return err
	})
}

// TestConfigMapMount creates a ConfigMap and a Pod that reads it via an
// environment variable, confirming the virtual cluster can mount ConfigMaps.
func TestConfigMapMount(t *testing.T) {
	name := helpers.UniqueName("cm")
	ns := "vc-" + name
	defer helpers.DumpDebug(t, ns)

	vcKubeconfig := helpers.CreateVCluster(t, name)

	// Create a ConfigMap with a key.
	helpers.MustKubectl(t, vcKubeconfig,
		"create", "configmap", "app-config",
		"--from-literal=MESSAGE=hello-from-configmap")

	// Run a pod that reads the ConfigMap key as an env var and prints it.
	podYAML := `
apiVersion: v1
kind: Pod
metadata:
  name: cm-reader
spec:
  restartPolicy: Never
  containers:
  - name: reader
    image: busybox:latest
    command: ["sh", "-c", "echo $MESSAGE && sleep 5"]
    env:
    - name: MESSAGE
      valueFrom:
        configMapKeyRef:
          name: app-config
          key: MESSAGE
`
	helpers.MustKubectlApply(t, vcKubeconfig, podYAML)

	// Wait for the pod to complete.
	helpers.MustWaitFor(t, 5*time.Minute, 5*time.Second, func() error {
		out, err := helpers.Kubectl(t, vcKubeconfig,
			"get", "pod", "cm-reader", "-o", "jsonpath={.status.phase}")
		if err != nil {
			return err
		}
		phase := strings.TrimSpace(out)
		if phase == "Succeeded" {
			return nil
		}
		return fmt.Errorf("pod phase: %s", phase)
	})

	// Verify the pod logged the expected message.
	logs, err := helpers.Kubectl(t, vcKubeconfig, "logs", "cm-reader")
	if err != nil {
		t.Fatalf("getting pod logs: %v", err)
	}
	if !strings.Contains(logs, "hello-from-configmap") {
		t.Errorf("expected ConfigMap value in pod logs; got:\n%s", logs)
	}
}
