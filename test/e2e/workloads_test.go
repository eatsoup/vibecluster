//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/eatsoup/vibecluster/test/e2e/helpers"
)

// TestNginxDeployment deploys nginx into the shared vcluster, waits for the
// pod to reach Running, and confirms the syncer mirrors it into the host
// namespace under the translated name.
func TestNginxDeployment(t *testing.T) {
	t.Parallel()
	ns := "vc-" + helpers.SharedVCName
	defer helpers.DumpDebug(t, ns)

	deplName := helpers.UniqueName("nginx")

	helpers.MustKubectl(t, helpers.SharedVCKubeconfig,
		"create", "deployment", deplName, "--image=nginx:alpine", "--replicas=1")
	t.Cleanup(func() {
		helpers.Kubectl(t, helpers.SharedVCKubeconfig, "delete", "deployment", deplName, "--ignore-not-found") //nolint:errcheck
	})

	helpers.WaitForPodRunning(t, helpers.SharedVCKubeconfig, "default", "app="+deplName, 5*time.Minute)

	// Syncer name translation: <clustername>-x-<podname>-x-<namespace>
	prefix := helpers.SharedVCName + "-x-" + deplName
	helpers.MustWaitFor(t, 2*time.Minute, 5*time.Second, func() error {
		out, err := helpers.Kubectl(t, helpers.HostKubeconfig,
			"get", "pods", "-n", ns, "-o", "jsonpath={.items[*].metadata.name}")
		if err != nil {
			return err
		}
		for _, podName := range strings.Fields(out) {
			if strings.HasPrefix(podName, prefix) {
				return nil
			}
		}
		return fmt.Errorf("no host pod with prefix %q; got: %s", prefix, out)
	})
}

// TestPodToPodCommunication deploys two workloads in the shared vcluster and
// verifies that a curl from one pod reaches the other via ClusterIP service.
func TestPodToPodCommunication(t *testing.T) {
	t.Parallel()
	defer helpers.DumpDebug(t, "vc-"+helpers.SharedVCName)

	svcName := helpers.UniqueName("svc")
	clientName := helpers.UniqueName("client")

	helpers.MustKubectl(t, helpers.SharedVCKubeconfig,
		"create", "deployment", svcName, "--image=nginx:alpine")
	helpers.MustKubectl(t, helpers.SharedVCKubeconfig,
		"expose", "deployment", svcName, "--port=80", "--name="+svcName)
	helpers.MustKubectl(t, helpers.SharedVCKubeconfig,
		"run", clientName, "--image=curlimages/curl:latest",
		"--restart=Never", "--command", "--", "sleep", "3600")
	t.Cleanup(func() {
		helpers.Kubectl(t, helpers.SharedVCKubeconfig, "delete", "deployment", svcName, "--ignore-not-found")   //nolint:errcheck
		helpers.Kubectl(t, helpers.SharedVCKubeconfig, "delete", "svc", svcName, "--ignore-not-found")         //nolint:errcheck
		helpers.Kubectl(t, helpers.SharedVCKubeconfig, "delete", "pod", clientName, "--ignore-not-found")       //nolint:errcheck
	})

	helpers.WaitForPodRunning(t, helpers.SharedVCKubeconfig, "default", "app="+svcName, 5*time.Minute)
	helpers.MustWaitFor(t, 3*time.Minute, 5*time.Second, func() error {
		out, _ := helpers.Kubectl(t, helpers.SharedVCKubeconfig,
			"get", "pod", clientName, "-o", "jsonpath={.status.phase}")
		if strings.TrimSpace(out) == "Running" {
			return nil
		}
		return fmt.Errorf("client pod phase: %s", out)
	})

	helpers.MustWaitFor(t, 60*time.Second, 5*time.Second, func() error {
		out, err := helpers.KubectlExec(t, helpers.SharedVCKubeconfig,
			"default", clientName, clientName,
			"curl", "-sf", "--max-time", "5", "http://"+svcName+"/")
		if err != nil {
			return fmt.Errorf("curl: %v", err)
		}
		if !strings.Contains(out, "Welcome to nginx") {
			return fmt.Errorf("unexpected response: %s", out)
		}
		return nil
	})
}

// TestDNSResolution verifies FQDN resolution inside the shared vcluster.
func TestDNSResolution(t *testing.T) {
	t.Parallel()
	defer helpers.DumpDebug(t, "vc-"+helpers.SharedVCName)

	webName := helpers.UniqueName("web")
	resolverName := helpers.UniqueName("res")

	helpers.MustKubectl(t, helpers.SharedVCKubeconfig,
		"create", "deployment", webName, "--image=nginx:alpine")
	helpers.MustKubectl(t, helpers.SharedVCKubeconfig,
		"expose", "deployment", webName, "--port=80", "--name="+webName)
	helpers.MustKubectl(t, helpers.SharedVCKubeconfig,
		"run", resolverName, "--image=curlimages/curl:latest",
		"--restart=Never", "--command", "--", "sleep", "3600")
	t.Cleanup(func() {
		helpers.Kubectl(t, helpers.SharedVCKubeconfig, "delete", "deployment", webName, "--ignore-not-found") //nolint:errcheck
		helpers.Kubectl(t, helpers.SharedVCKubeconfig, "delete", "svc", webName, "--ignore-not-found")       //nolint:errcheck
		helpers.Kubectl(t, helpers.SharedVCKubeconfig, "delete", "pod", resolverName, "--ignore-not-found")  //nolint:errcheck
	})

	helpers.WaitForPodRunning(t, helpers.SharedVCKubeconfig, "default", "app="+webName, 5*time.Minute)
	helpers.MustWaitFor(t, 3*time.Minute, 5*time.Second, func() error {
		out, _ := helpers.Kubectl(t, helpers.SharedVCKubeconfig,
			"get", "pod", resolverName, "-o", "jsonpath={.status.phase}")
		if strings.TrimSpace(out) == "Running" {
			return nil
		}
		return fmt.Errorf("resolver pod phase: %s", out)
	})

	fqdn := webName + ".default.svc.cluster.local"
	helpers.MustWaitFor(t, 60*time.Second, 5*time.Second, func() error {
		_, err := helpers.KubectlExec(t, helpers.SharedVCKubeconfig,
			"default", resolverName, resolverName,
			"curl", "-sf", "--max-time", "5", "http://"+fqdn+"/")
		return err
	})
}

// TestConfigMapMount creates a ConfigMap and Pod in the shared vcluster that
// reads it via an environment variable.
func TestConfigMapMount(t *testing.T) {
	t.Parallel()
	defer helpers.DumpDebug(t, "vc-"+helpers.SharedVCName)

	cmName := helpers.UniqueName("cm")
	podName := helpers.UniqueName("reader")

	helpers.MustKubectl(t, helpers.SharedVCKubeconfig,
		"create", "configmap", cmName, "--from-literal=MESSAGE=hello-"+cmName)
	t.Cleanup(func() {
		helpers.Kubectl(t, helpers.SharedVCKubeconfig, "delete", "configmap", cmName, "--ignore-not-found") //nolint:errcheck
		helpers.Kubectl(t, helpers.SharedVCKubeconfig, "delete", "pod", podName, "--ignore-not-found")      //nolint:errcheck
	})

	helpers.MustKubectlApply(t, helpers.SharedVCKubeconfig, fmt.Sprintf(`
apiVersion: v1
kind: Pod
metadata:
  name: %s
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
          name: %s
          key: MESSAGE
`, podName, cmName))

	helpers.MustWaitFor(t, 5*time.Minute, 5*time.Second, func() error {
		out, err := helpers.Kubectl(t, helpers.SharedVCKubeconfig,
			"get", "pod", podName, "-o", "jsonpath={.status.phase}")
		if err != nil {
			return err
		}
		if strings.TrimSpace(out) == "Succeeded" {
			return nil
		}
		return fmt.Errorf("pod phase: %s", out)
	})

	logs, err := helpers.Kubectl(t, helpers.SharedVCKubeconfig, "logs", podName)
	if err != nil {
		t.Fatalf("getting pod logs: %v", err)
	}
	if !strings.Contains(logs, "hello-"+cmName) {
		t.Errorf("expected ConfigMap value in pod logs; got:\n%s", logs)
	}
}
