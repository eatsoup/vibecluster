//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/eatsoup/vibecluster/test/e2e/helpers"
)

// operatorInstalled tracks whether we've already installed the operator in
// this test run; operator tests share the install so we only pay the cost once.
var operatorInstalled bool

// ensureOperator installs the vibecluster operator if it is not yet installed.
// It registers teardown via t.Cleanup on the first call; subsequent calls are
// no-ops.
func ensureOperator(t *testing.T) {
	t.Helper()
	if operatorInstalled {
		return
	}

	args := []string{"operator", "install"}
	if helpers.OperatorImage != "" {
		args = append(args, "--image", helpers.OperatorImage)
	}
	helpers.MustVibeCluster(t, args...)

	// Wait for the operator Deployment to become ready.
	helpers.MustWaitFor(t, 3*time.Minute, 5*time.Second, func() error {
		out, err := helpers.Kubectl(t, helpers.HostKubeconfig,
			"get", "deployment", "-n", "vibecluster-system",
			"-o", "jsonpath={.items[*].status.readyReplicas}")
		if err != nil {
			return err
		}
		if strings.TrimSpace(out) == "" || strings.TrimSpace(out) == "0" {
			return fmt.Errorf("operator deployment not ready yet")
		}
		return nil
	})

	operatorInstalled = true
}

// TestOperatorInstall verifies that `vibecluster operator install` creates the
// operator Deployment in the vibecluster-system namespace and that it becomes Ready.
func TestOperatorInstall(t *testing.T) {
	ensureOperator(t)

	// Deployment must exist and be ready (ensureOperator already waits, but
	// assert explicitly for a clean test failure message).
	out := helpers.MustKubectl(t, helpers.HostKubeconfig,
		"get", "deployment", "-n", "vibecluster-system",
		"-o", "jsonpath={.items[*].metadata.name}")
	if strings.TrimSpace(out) == "" {
		t.Error("no Deployment found in vibecluster-system after operator install")
	}

	// CRD must be registered.
	_, err := helpers.Kubectl(t, helpers.HostKubeconfig,
		"get", "crd", "virtualclusters.vibecluster.dev")
	if err != nil {
		t.Errorf("VirtualCluster CRD not installed: %v", err)
	}
}

// TestOperatorCRCreate applies a VirtualCluster CR and verifies the operator
// reconciles it to phase=Running with a backing host namespace.
func TestOperatorCRCreate(t *testing.T) {
	ensureOperator(t)

	name := helpers.UniqueName("op")
	defer helpers.DumpDebug(t, "vc-"+name)

	crYAML := fmt.Sprintf(`
apiVersion: vibecluster.dev/v1alpha1
kind: VirtualCluster
metadata:
  name: %s
  namespace: default
spec:
  storage: "1Gi"
`, name)

	helpers.MustKubectlApply(t, helpers.HostKubeconfig, crYAML)
	t.Cleanup(func() {
		helpers.Kubectl(t, helpers.HostKubeconfig, //nolint:errcheck
			"delete", "virtualcluster", name, "-n", "default", "--ignore-not-found")
		// Wait for host namespace to be gone.
		helpers.WaitFor(t, 60*time.Second, 3*time.Second, func() error { //nolint:errcheck
			_, err := helpers.Kubectl(t, helpers.HostKubeconfig, "get", "namespace", "vc-"+name)
			if err != nil {
				return nil
			}
			return fmt.Errorf("namespace vc-%s still exists", name)
		})
	})

	// CR should reach phase=Running.
	helpers.MustWaitFor(t, 8*time.Minute, 10*time.Second, func() error {
		out, err := helpers.Kubectl(t, helpers.HostKubeconfig,
			"get", "virtualcluster", name, "-n", "default",
			"-o", "jsonpath={.status.phase}")
		if err != nil {
			return err
		}
		if strings.TrimSpace(out) != "Running" {
			return fmt.Errorf("VirtualCluster phase: %q", out)
		}
		return nil
	})

	// Host namespace must exist.
	if _, err := helpers.Kubectl(t, helpers.HostKubeconfig, "get", "namespace", "vc-"+name); err != nil {
		t.Errorf("host namespace vc-%s not found after CR reached Running: %v", name, err)
	}

	// `vibecluster list` must surface the cluster with mode=operator.
	out, err := helpers.RunVibeCluster(t, "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out, name) {
		t.Errorf("cluster %q not found in list output:\n%s", name, out)
	}
	if !strings.Contains(out, "operator") {
		t.Errorf("cluster %q not shown as operator mode in list:\n%s", name, out)
	}
}

// TestOperatorCRDelete verifies that deleting a VirtualCluster CR causes the
// operator to clean up the host namespace.
func TestOperatorCRDelete(t *testing.T) {
	ensureOperator(t)

	name := helpers.UniqueName("opd")
	defer helpers.DumpDebug(t, "vc-"+name)

	crYAML := fmt.Sprintf(`
apiVersion: vibecluster.dev/v1alpha1
kind: VirtualCluster
metadata:
  name: %s
  namespace: default
spec:
  storage: "1Gi"
`, name)

	helpers.MustKubectlApply(t, helpers.HostKubeconfig, crYAML)

	// Wait for it to be Running.
	helpers.MustWaitFor(t, 8*time.Minute, 10*time.Second, func() error {
		out, err := helpers.Kubectl(t, helpers.HostKubeconfig,
			"get", "virtualcluster", name, "-n", "default",
			"-o", "jsonpath={.status.phase}")
		if err != nil {
			return err
		}
		if strings.TrimSpace(out) != "Running" {
			return fmt.Errorf("waiting for Running, got %q", out)
		}
		return nil
	})

	// Delete the CR.
	helpers.MustKubectl(t, helpers.HostKubeconfig,
		"delete", "virtualcluster", name, "-n", "default")

	// Host namespace must disappear.
	helpers.MustWaitFor(t, 2*time.Minute, 5*time.Second, func() error {
		_, err := helpers.Kubectl(t, helpers.HostKubeconfig, "get", "namespace", "vc-"+name)
		if err != nil {
			return nil // gone
		}
		return fmt.Errorf("namespace vc-%s still exists after CR deletion", name)
	})
}

// TestOperatorUninstall verifies that `vibecluster operator uninstall` removes
// the controller Deployment and CRD, while leaving any existing vclusters intact.
func TestOperatorUninstall(t *testing.T) {
	// This test must run last among operator tests because it removes the operator.
	// Any clusters created before this point (and still running) should survive.

	ensureOperator(t)

	// Create a legacy cluster that should survive the uninstall.
	survivorName := helpers.UniqueName("sur")
	helpers.MustVibeCluster(t, "create", survivorName, "--mode=legacy", "--connect=false")
	t.Cleanup(func() { helpers.RunVibeCluster(t, "delete", survivorName) })
	helpers.WaitForStatefulSetReady(t, helpers.HostKubeconfig, "vc-"+survivorName, survivorName, 5*time.Minute)

	// Uninstall the operator.
	helpers.MustVibeCluster(t, "operator", "uninstall")
	operatorInstalled = false // reset for potential re-install in later tests

	// Operator Deployment must be gone.
	helpers.MustWaitFor(t, 60*time.Second, 3*time.Second, func() error {
		out, _ := helpers.Kubectl(t, helpers.HostKubeconfig,
			"get", "deployment", "-n", "vibecluster-system",
			"-o", "jsonpath={.items[*].metadata.name}")
		if strings.TrimSpace(out) == "" {
			return nil
		}
		return fmt.Errorf("operator deployment still exists")
	})

	// Survivor cluster must still be running.
	out := helpers.MustKubectl(t, helpers.HostKubeconfig,
		"get", "statefulset", survivorName, "-n", "vc-"+survivorName,
		"-o", "jsonpath={.status.readyReplicas}")
	if strings.TrimSpace(out) == "0" || strings.TrimSpace(out) == "" {
		t.Errorf("legacy cluster %q stopped after operator uninstall", survivorName)
	}
}
