//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/eatsoup/vibecluster/test/e2e/helpers"
)

// TestPodNameTranslation verifies the syncer's naming convention:
// a pod named <pod> in virtual namespace <ns> appears in the host as
// <clustername>-x-<pod>-x-<ns> inside vc-<clustername>.
func TestPodNameTranslation(t *testing.T) {
	name := helpers.UniqueName("syn")
	ns := "vc-" + name
	defer helpers.DumpDebug(t, ns)

	vcKubeconfig := helpers.CreateVCluster(t, name)

	helpers.MustKubectl(t, vcKubeconfig,
		"run", "myapp", "--image=nginx:alpine", "--restart=Never")

	helpers.MustWaitFor(t, 5*time.Minute, 5*time.Second, func() error {
		out, err := helpers.Kubectl(t, helpers.HostKubeconfig,
			"get", "pods", "-n", ns, "-o", "jsonpath={.items[*].metadata.name}")
		if err != nil {
			return err
		}
		translated := name + "-x-myapp-x-default"
		for _, podName := range strings.Fields(out) {
			if podName == translated {
				return nil
			}
		}
		return fmt.Errorf("translated pod %q not found in host ns %s; got: %s", translated, ns, out)
	})
}

// TestServiceSync verifies that a Service created in the virtual cluster is
// mirrored into the host namespace with the translated name.
func TestServiceSync(t *testing.T) {
	name := helpers.UniqueName("svc")
	ns := "vc-" + name
	defer helpers.DumpDebug(t, ns)

	vcKubeconfig := helpers.CreateVCluster(t, name)

	helpers.MustKubectl(t, vcKubeconfig,
		"create", "service", "clusterip", "my-svc", "--tcp=80:80")

	translated := name + "-x-my-svc-x-default"
	helpers.MustWaitFor(t, 2*time.Minute, 5*time.Second, func() error {
		_, err := helpers.Kubectl(t, helpers.HostKubeconfig,
			"get", "svc", translated, "-n", ns)
		return err
	})
}

// TestConfigMapSync verifies that a ConfigMap created in the virtual cluster
// is mirrored into the host namespace with the translated name and correct data.
func TestConfigMapSync(t *testing.T) {
	name := helpers.UniqueName("cms")
	ns := "vc-" + name
	defer helpers.DumpDebug(t, ns)

	vcKubeconfig := helpers.CreateVCluster(t, name)

	helpers.MustKubectl(t, vcKubeconfig,
		"create", "configmap", "test-cm", "--from-literal=key=value123")

	translated := name + "-x-test-cm-x-default"
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

// TestDeletePropagation verifies that deleting a resource in the virtual
// cluster causes the syncer to remove its host shadow.
func TestDeletePropagation(t *testing.T) {
	name := helpers.UniqueName("del")
	ns := "vc-" + name
	defer helpers.DumpDebug(t, ns)

	vcKubeconfig := helpers.CreateVCluster(t, name)

	// Create and wait for sync.
	helpers.MustKubectl(t, vcKubeconfig,
		"create", "configmap", "ephemeral-cm", "--from-literal=x=1")
	translated := name + "-x-ephemeral-cm-x-default"
	helpers.MustWaitFor(t, 2*time.Minute, 5*time.Second, func() error {
		_, err := helpers.Kubectl(t, helpers.HostKubeconfig,
			"get", "configmap", translated, "-n", ns)
		return err
	})

	// Delete the virtual resource.
	helpers.MustKubectl(t, vcKubeconfig, "delete", "configmap", "ephemeral-cm")

	// Host shadow must disappear.
	helpers.MustWaitFor(t, 2*time.Minute, 5*time.Second, func() error {
		_, err := helpers.Kubectl(t, helpers.HostKubeconfig,
			"get", "configmap", translated, "-n", ns)
		if err != nil {
			return nil // gone — expected
		}
		return fmt.Errorf("host shadow %s still exists", translated)
	})
}

// TestConfigMapUpdatePropagation verifies that updating a ConfigMap in the
// virtual cluster propagates the change to the host shadow.
func TestConfigMapUpdatePropagation(t *testing.T) {
	name := helpers.UniqueName("upd")
	ns := "vc-" + name
	defer helpers.DumpDebug(t, ns)

	vcKubeconfig := helpers.CreateVCluster(t, name)

	helpers.MustKubectl(t, vcKubeconfig,
		"create", "configmap", "mutable-cm", "--from-literal=rev=v1")

	translated := name + "-x-mutable-cm-x-default"
	helpers.MustWaitFor(t, 2*time.Minute, 5*time.Second, func() error {
		_, err := helpers.Kubectl(t, helpers.HostKubeconfig,
			"get", "configmap", translated, "-n", ns)
		return err
	})

	// Patch the virtual ConfigMap.
	helpers.MustKubectl(t, vcKubeconfig,
		"patch", "configmap", "mutable-cm",
		"--type=merge", "-p", `{"data":{"rev":"v2"}}`)

	// Verify the host shadow picks up the change.
	helpers.MustWaitFor(t, 2*time.Minute, 5*time.Second, func() error {
		out, err := helpers.Kubectl(t, helpers.HostKubeconfig,
			"get", "configmap", translated, "-n", ns,
			"-o", "jsonpath={.data.rev}")
		if err != nil {
			return err
		}
		if strings.TrimSpace(out) != "v2" {
			return fmt.Errorf("expected rev=v2, got %q", out)
		}
		return nil
	})
}

// TestNodeSyncHostToVirtual verifies that the syncer mirrors host nodes into
// the virtual cluster as read-only Node objects.
func TestNodeSyncHostToVirtual(t *testing.T) {
	name := helpers.UniqueName("nds")
	defer helpers.DumpDebug(t, "vc-"+name)

	vcKubeconfig := helpers.CreateVCluster(t, name)

	// Get node count from host.
	hostOut := helpers.MustKubectl(t, helpers.HostKubeconfig, "get", "nodes", "-o", "name")
	hostNodeCount := len(strings.Fields(strings.TrimSpace(hostOut)))

	// Virtual cluster should see the same nodes.
	helpers.MustWaitFor(t, 2*time.Minute, 5*time.Second, func() error {
		vcOut, err := helpers.Kubectl(t, vcKubeconfig, "get", "nodes", "-o", "name")
		if err != nil {
			return err
		}
		vcNodeCount := len(strings.Fields(strings.TrimSpace(vcOut)))
		if vcNodeCount < hostNodeCount {
			return fmt.Errorf("virtual cluster has %d nodes, expected at least %d", vcNodeCount, hostNodeCount)
		}
		return nil
	})
}
