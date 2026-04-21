//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/eatsoup/vibecluster/test/e2e/helpers"
)

// TestPodNameTranslation verifies that a pod in the shared vcluster appears in
// the host namespace under the translated name <clustername>-x-<pod>-x-<ns>.
func TestPodNameTranslation(t *testing.T) {
	t.Parallel()
	ns := "vc-" + helpers.SharedVCName
	defer helpers.DumpDebug(t, ns)

	podName := helpers.UniqueName("myapp")
	helpers.MustKubectl(t, helpers.SharedVCKubeconfig,
		"run", podName, "--image=nginx:alpine", "--restart=Never")
	t.Cleanup(func() {
		helpers.Kubectl(t, helpers.SharedVCKubeconfig, "delete", "pod", podName, "--ignore-not-found", "--wait=false") //nolint:errcheck
	})

	translated := helpers.SharedVCName + "-x-" + podName + "-x-default"
	helpers.MustWaitFor(t, 5*time.Minute, 5*time.Second, func() error {
		out, err := helpers.Kubectl(t, helpers.HostKubeconfig,
			"get", "pods", "-n", ns, "-o", "jsonpath={.items[*].metadata.name}")
		if err != nil {
			return err
		}
		for _, n := range strings.Fields(out) {
			if n == translated {
				return nil
			}
		}
		return fmt.Errorf("translated pod %q not found; got: %s", translated, out)
	})
}

// TestServiceSync verifies that a Service in the shared vcluster is mirrored
// into the host namespace.
func TestServiceSync(t *testing.T) {
	t.Parallel()
	ns := "vc-" + helpers.SharedVCName
	defer helpers.DumpDebug(t, ns)

	svcName := helpers.UniqueName("mysvc")
	helpers.MustKubectl(t, helpers.SharedVCKubeconfig,
		"create", "service", "clusterip", svcName, "--tcp=80:80")
	t.Cleanup(func() {
		helpers.Kubectl(t, helpers.SharedVCKubeconfig, "delete", "svc", svcName, "--ignore-not-found") //nolint:errcheck
	})

	translated := helpers.SharedVCName + "-x-" + svcName + "-x-default"
	helpers.MustWaitFor(t, 2*time.Minute, 5*time.Second, func() error {
		_, err := helpers.Kubectl(t, helpers.HostKubeconfig,
			"get", "svc", translated, "-n", ns)
		return err
	})
}

// TestConfigMapSync verifies that a ConfigMap in the shared vcluster is
// mirrored into the host namespace with the correct data.
func TestConfigMapSync(t *testing.T) {
	t.Parallel()
	ns := "vc-" + helpers.SharedVCName
	defer helpers.DumpDebug(t, ns)

	cmName := helpers.UniqueName("cm")
	helpers.MustKubectl(t, helpers.SharedVCKubeconfig,
		"create", "configmap", cmName, "--from-literal=key=value123")
	t.Cleanup(func() {
		helpers.Kubectl(t, helpers.SharedVCKubeconfig, "delete", "configmap", cmName, "--ignore-not-found") //nolint:errcheck
	})

	translated := helpers.SharedVCName + "-x-" + cmName + "-x-default"
	helpers.MustWaitFor(t, 2*time.Minute, 5*time.Second, func() error {
		out, err := helpers.Kubectl(t, helpers.HostKubeconfig,
			"get", "configmap", translated, "-n", ns,
			"-o", "jsonpath={.data.key}")
		if err != nil {
			return err
		}
		if strings.TrimSpace(out) != "value123" {
			return fmt.Errorf("expected value123, got %q", out)
		}
		return nil
	})
}

// TestDeletePropagation verifies that deleting a virtual resource removes its
// host shadow.
func TestDeletePropagation(t *testing.T) {
	t.Parallel()
	ns := "vc-" + helpers.SharedVCName
	defer helpers.DumpDebug(t, ns)

	cmName := helpers.UniqueName("eph")
	helpers.MustKubectl(t, helpers.SharedVCKubeconfig,
		"create", "configmap", cmName, "--from-literal=x=1")

	translated := helpers.SharedVCName + "-x-" + cmName + "-x-default"
	helpers.MustWaitFor(t, 2*time.Minute, 5*time.Second, func() error {
		_, err := helpers.Kubectl(t, helpers.HostKubeconfig, "get", "configmap", translated, "-n", ns)
		return err
	})

	helpers.MustKubectl(t, helpers.SharedVCKubeconfig, "delete", "configmap", cmName)

	helpers.MustWaitFor(t, 2*time.Minute, 5*time.Second, func() error {
		_, err := helpers.Kubectl(t, helpers.HostKubeconfig, "get", "configmap", translated, "-n", ns)
		if err != nil {
			return nil
		}
		return fmt.Errorf("host shadow %s still exists", translated)
	})
}

// TestConfigMapUpdatePropagation verifies that updating a virtual ConfigMap
// propagates to the host shadow.
func TestConfigMapUpdatePropagation(t *testing.T) {
	t.Parallel()
	ns := "vc-" + helpers.SharedVCName
	defer helpers.DumpDebug(t, ns)

	cmName := helpers.UniqueName("upd")
	helpers.MustKubectl(t, helpers.SharedVCKubeconfig,
		"create", "configmap", cmName, "--from-literal=rev=v1")
	t.Cleanup(func() {
		helpers.Kubectl(t, helpers.SharedVCKubeconfig, "delete", "configmap", cmName, "--ignore-not-found") //nolint:errcheck
	})

	translated := helpers.SharedVCName + "-x-" + cmName + "-x-default"
	helpers.MustWaitFor(t, 2*time.Minute, 5*time.Second, func() error {
		_, err := helpers.Kubectl(t, helpers.HostKubeconfig, "get", "configmap", translated, "-n", ns)
		return err
	})

	helpers.MustKubectl(t, helpers.SharedVCKubeconfig,
		"patch", "configmap", cmName, "--type=merge", "-p", `{"data":{"rev":"v2"}}`)

	helpers.MustWaitFor(t, 2*time.Minute, 5*time.Second, func() error {
		out, err := helpers.Kubectl(t, helpers.HostKubeconfig,
			"get", "configmap", translated, "-n", ns, "-o", "jsonpath={.data.rev}")
		if err != nil {
			return err
		}
		if strings.TrimSpace(out) != "v2" {
			return fmt.Errorf("expected rev=v2, got %q", out)
		}
		return nil
	})
}

// TestNodeSyncHostToVirtual verifies that host nodes appear in the shared
// vcluster as read-only Node objects.
func TestNodeSyncHostToVirtual(t *testing.T) {
	t.Parallel()

	hostOut := helpers.MustKubectl(t, helpers.HostKubeconfig, "get", "nodes", "-o", "name")
	hostNodeCount := len(strings.Fields(strings.TrimSpace(hostOut)))

	helpers.MustWaitFor(t, 2*time.Minute, 5*time.Second, func() error {
		vcOut, err := helpers.Kubectl(t, helpers.SharedVCKubeconfig, "get", "nodes", "-o", "name")
		if err != nil {
			return err
		}
		vcNodeCount := len(strings.Fields(strings.TrimSpace(vcOut)))
		if vcNodeCount < hostNodeCount {
			return fmt.Errorf("virtual cluster has %d nodes, want at least %d", vcNodeCount, hostNodeCount)
		}
		return nil
	})
}
