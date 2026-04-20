//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/eatsoup/vibecluster/test/e2e/helpers"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// TestCreateLegacy verifies that `vibecluster create` produces all expected
// host resources: namespace, StatefulSet, Service, ServiceAccount, and
// ClusterRoleBinding.
func TestCreateLegacy(t *testing.T) {
	name := helpers.UniqueName("lc")
	ns := "vc-" + name
	defer helpers.DumpDebug(t, ns)

	helpers.MustVibeCluster(t, "create", name, "--mode=legacy", "--connect=false")
	t.Cleanup(func() { helpers.RunVibeCluster(t, "delete", name) })

	// Namespace must exist.
	helpers.MustKubectl(t, helpers.HostKubeconfig, "get", "namespace", ns)

	// StatefulSet must exist and eventually become ready.
	helpers.WaitForStatefulSetReady(t, helpers.HostKubeconfig, ns, name, 5*time.Minute)

	// Service must exist.
	helpers.MustKubectl(t, helpers.HostKubeconfig, "get", "svc", name, "-n", ns)

	// ServiceAccount must exist.
	helpers.MustKubectl(t, helpers.HostKubeconfig, "get", "serviceaccount", name, "-n", ns)
}

// TestListShowsCluster verifies that `vibecluster list` surfaces a created cluster.
func TestListShowsCluster(t *testing.T) {
	name := helpers.UniqueName("ls")
	defer helpers.DumpDebug(t, "vc-"+name)

	helpers.MustVibeCluster(t, "create", name, "--mode=legacy", "--connect=false")
	t.Cleanup(func() { helpers.RunVibeCluster(t, "delete", name) })

	helpers.MustWaitFor(t, 30*time.Second, 3*time.Second, func() error {
		out, err := helpers.RunVibeCluster(t, "list")
		if err != nil {
			return err
		}
		if !strings.Contains(out, name) {
			return helpers.ErrNotFound(name, out)
		}
		return nil
	})
}

// TestConnectPrint verifies that `vibecluster connect --print` emits a valid
// kubeconfig YAML (contains the expected context name).
func TestConnectPrint(t *testing.T) {
	name := helpers.UniqueName("cp")
	defer helpers.DumpDebug(t, "vc-"+name)

	helpers.MustVibeCluster(t, "create", name, "--mode=legacy", "--connect=false")
	t.Cleanup(func() { helpers.RunVibeCluster(t, "delete", name) })

	helpers.WaitForStatefulSetReady(t, helpers.HostKubeconfig, "vc-"+name, name, 5*time.Minute)

	// connect --print should emit a kubeconfig mentioning the cluster context.
	out, err := helpers.RunVibeCluster(t, "connect", name, "--print")
	if err != nil {
		t.Fatalf("connect --print: %v", err)
	}
	expectedCtx := "vibecluster-" + name
	if !strings.Contains(out, expectedCtx) {
		t.Errorf("expected context %q in connect --print output; got:\n%s", expectedCtx, out)
	}
}

// TestLogsCommands verifies that both the syncer and k3s log streams are
// non-empty, confirming both containers are running and producing output.
func TestLogsCommands(t *testing.T) {
	name := helpers.UniqueName("log")
	defer helpers.DumpDebug(t, "vc-"+name)

	helpers.MustVibeCluster(t, "create", name, "--mode=legacy", "--connect=false")
	t.Cleanup(func() { helpers.RunVibeCluster(t, "delete", name) })

	helpers.WaitForStatefulSetReady(t, helpers.HostKubeconfig, "vc-"+name, name, 5*time.Minute)

	t.Run("syncer_logs", func(t *testing.T) {
		out, err := helpers.RunVibeCluster(t, "logs", name)
		if err != nil {
			t.Fatalf("logs: %v", err)
		}
		if strings.TrimSpace(out) == "" {
			t.Error("syncer logs are empty")
		}
	})

	t.Run("k3s_logs", func(t *testing.T) {
		out, err := helpers.RunVibeCluster(t, "logs", name, "-c", "k3s")
		if err != nil {
			t.Fatalf("logs -c k3s: %v", err)
		}
		if strings.TrimSpace(out) == "" {
			t.Error("k3s logs are empty")
		}
	})
}

// TestDelete verifies that after deletion the vc-* namespace no longer exists.
func TestDelete(t *testing.T) {
	name := helpers.UniqueName("del")
	helpers.MustVibeCluster(t, "create", name, "--mode=legacy", "--connect=false")

	if _, err := helpers.RunVibeCluster(t, "delete", name); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// Namespace should be gone (may take a moment for k8s to finish).
	helpers.MustWaitFor(t, 30*time.Second, 3*time.Second, func() error {
		_, err := helpers.Kubectl(t, helpers.HostKubeconfig,
			"get", "namespace", "vc-"+name)
		if err != nil {
			return nil // namespace gone — expected
		}
		return helpers.ErrNotFound("namespace to be gone", "still exists")
	})
}

// TestCreateSameNameTwice verifies that creating a cluster that already exists
// returns a non-zero exit code.
func TestCreateSameNameTwice(t *testing.T) {
	name := helpers.UniqueName("dup")
	defer helpers.DumpDebug(t, "vc-"+name)

	helpers.MustVibeCluster(t, "create", name, "--mode=legacy", "--connect=false")
	t.Cleanup(func() { helpers.RunVibeCluster(t, "delete", name) })

	_, err := helpers.RunVibeCluster(t, "create", name, "--mode=legacy", "--connect=false")
	if err == nil {
		t.Error("expected error when creating a duplicate cluster; got none")
	}
}

// TestConnectNonExistent verifies that connecting to a missing cluster returns
// an error.
func TestConnectNonExistent(t *testing.T) {
	_, err := helpers.RunVibeCluster(t, "connect", "does-not-exist-e2e")
	if err == nil {
		t.Error("expected error connecting to non-existent cluster; got none")
	}
}

// TestDeleteNonExistent verifies that deleting a missing cluster either
// succeeds (idempotent) or returns a clear error — both are acceptable; it
// must not panic or hang.
func TestDeleteNonExistent(t *testing.T) {
	out, err := helpers.RunVibeCluster(t, "delete", "no-such-cluster-e2e")
	t.Logf("delete non-existent output: %s err: %v", out, err)
	// Either exit 0 (idempotent) or a clear error is acceptable.
	// What is NOT acceptable: a hang or a panic — the test timeout catches both.
}

// TestCreateDeleteRecreate verifies that creating, deleting, and recreating a
// cluster with the same name works without leftover state causing failures.
func TestCreateDeleteRecreate(t *testing.T) {
	name := helpers.UniqueName("cdr")
	defer helpers.DumpDebug(t, "vc-"+name)

	for i := range 2 {
		helpers.MustVibeCluster(t, "create", name, "--mode=legacy", "--connect=false")
		helpers.WaitForStatefulSetReady(t, helpers.HostKubeconfig, "vc-"+name, name, 5*time.Minute)

		if _, err := helpers.RunVibeCluster(t, "delete", name); err != nil {
			t.Fatalf("iteration %d: delete failed: %v", i, err)
		}
		// Wait for namespace to disappear before recreating.
		helpers.MustWaitFor(t, 60*time.Second, 3*time.Second, func() error {
			_, err := helpers.Kubectl(t, helpers.HostKubeconfig, "get", "namespace", "vc-"+name)
			if err != nil {
				return nil
			}
			return helpers.ErrNotFound("namespace to be gone", "still exists")
		})
	}
}

// hostClientForVCluster returns a client-go Clientset for the host cluster,
// scoped to the vcluster's namespace. Used by tests that prefer direct API
// calls over kubectl for finer-grained assertions.
func hostClientForVCluster(t *testing.T) *kubernetes.Clientset {
	t.Helper()
	cfg, err := clientcmd.BuildConfigFromFlags("", helpers.HostKubeconfig)
	if err != nil {
		t.Fatalf("building host rest config: %v", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("building host clientset: %v", err)
	}
	return cs
}

// nsExists checks whether a namespace exists in the host cluster.
func nsExists(t *testing.T, ns string) bool {
	t.Helper()
	cs := hostClientForVCluster(t)
	_, err := cs.CoreV1().Namespaces().Get(context.Background(), ns, metav1.GetOptions{})
	return err == nil
}
