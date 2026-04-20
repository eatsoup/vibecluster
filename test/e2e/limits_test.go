//go:build e2e

package e2e

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/eatsoup/vibecluster/test/e2e/helpers"
)

// TestResourceLimitsInstalled verifies that passing --cpu/--memory/--pods to
// `vibecluster create` installs a ResourceQuota and LimitRange on the
// vcluster's host namespace.
func TestResourceLimitsInstalled(t *testing.T) {
	t.Parallel()
	name := helpers.UniqueName("lim")
	ns := "vc-" + name
	defer helpers.DumpDebug(t, ns)

	helpers.MustVibeCluster(t, "create", name,
		"--mode=legacy", "--connect=false",
		"--cpu=2", "--memory=4Gi", "--pods=20")
	t.Cleanup(func() { helpers.RunVibeCluster(t, "delete", name) })

	helpers.WaitForStatefulSetReady(t, helpers.HostKubeconfig, ns, name, 5*time.Minute)

	// ResourceQuota must be present.
	helpers.MustWaitFor(t, 30*time.Second, 3*time.Second, func() error {
		out, err := helpers.Kubectl(t, helpers.HostKubeconfig,
			"get", "resourcequota", "-n", ns, "-o", "jsonpath={.items[*].metadata.name}")
		if err != nil {
			return err
		}
		if strings.TrimSpace(out) == "" {
			return fmt.Errorf("no ResourceQuota found in %s", ns)
		}
		return nil
	})

	// LimitRange must be present.
	helpers.MustWaitFor(t, 30*time.Second, 3*time.Second, func() error {
		out, err := helpers.Kubectl(t, helpers.HostKubeconfig,
			"get", "limitrange", "-n", ns, "-o", "jsonpath={.items[*].metadata.name}")
		if err != nil {
			return err
		}
		if strings.TrimSpace(out) == "" {
			return fmt.Errorf("no LimitRange found in %s", ns)
		}
		return nil
	})

	// ResourceQuota must encode the requested limits.
	out := helpers.MustKubectl(t, helpers.HostKubeconfig,
		"get", "resourcequota", "-n", ns, "-o", "yaml")
	for _, want := range []string{"cpu", "memory", "pods"} {
		if !strings.Contains(out, want) {
			t.Errorf("ResourceQuota YAML missing %q:\n%s", want, out)
		}
	}
}

// TestPodsCapEnforced verifies that the --pods cap is enforced: once the
// namespace pod count reaches the quota, additional pod creation is rejected
// by the Kubernetes admission control layer.
func TestPodsCapEnforced(t *testing.T) {
	t.Parallel()
	name := helpers.UniqueName("cap")
	ns := "vc-" + name
	defer helpers.DumpDebug(t, ns)

	// 2-pod cap: the k3s+syncer StatefulSet pod occupies one slot, so only
	// one additional pod should be admittable.
	helpers.MustVibeCluster(t, "create", name,
		"--mode=legacy", "--connect=false",
		"--pods=2")
	t.Cleanup(func() { helpers.RunVibeCluster(t, "delete", name) })

	helpers.WaitForStatefulSetReady(t, helpers.HostKubeconfig, ns, name, 5*time.Minute)

	// Try to create pods until one is rejected.
	quotaHit := false
	for i := range 5 {
		podYAML := fmt.Sprintf(`
apiVersion: v1
kind: Pod
metadata:
  name: filler-%d
  namespace: %s
spec:
  restartPolicy: Never
  containers:
  - name: c
    image: busybox:latest
    command: ["sleep","3600"]
    resources:
      requests:
        cpu: "1m"
        memory: "1Mi"
`, i, ns)
		if err := kubectlApplyHostRaw(t, podYAML); err != nil {
			if strings.Contains(err.Error(), "exceeded quota") ||
				strings.Contains(err.Error(), "forbidden") {
				quotaHit = true
				break
			}
			// Some other error; log and keep going.
			t.Logf("pod %d unexpected error: %v", i, err)
		}
	}

	if !quotaHit {
		t.Error("ResourceQuota not enforced: all 5 pods were admitted despite --pods=2 cap")
	}
}

// TestPodsWithoutRequestsAdmit verifies that the LimitRange installed alongside
// the ResourceQuota supplies default requests so that pods without explicit
// resource requests are still admitted under the quota.
func TestPodsWithoutRequestsAdmit(t *testing.T) {
	t.Parallel()
	name := helpers.UniqueName("lr")
	ns := "vc-" + name
	defer helpers.DumpDebug(t, ns)

	helpers.MustVibeCluster(t, "create", name,
		"--mode=legacy", "--connect=false",
		"--cpu=4", "--memory=8Gi", "--pods=50")
	t.Cleanup(func() { helpers.RunVibeCluster(t, "delete", name) })

	helpers.WaitForStatefulSetReady(t, helpers.HostKubeconfig, ns, name, 5*time.Minute)

	// A pod without resource requests should be admitted because the LimitRange
	// provides defaults. Without LimitRange, a quota namespace rejects any
	// pod that omits requests.
	podYAML := fmt.Sprintf(`
apiVersion: v1
kind: Pod
metadata:
  name: no-requests
  namespace: %s
spec:
  restartPolicy: Never
  containers:
  - name: c
    image: busybox:latest
    command: ["sleep","5"]
`, ns)

	if err := kubectlApplyHostRaw(t, podYAML); err != nil {
		t.Fatalf("pod without requests rejected despite LimitRange defaults: %v", err)
	}
}

// kubectlApplyHostRaw runs kubectl apply -f - against the host cluster,
// returning any error (including stderr) without calling t.Fatal.
func kubectlApplyHostRaw(t *testing.T, yaml string) error {
	t.Helper()
	cmd := exec.Command("kubectl", "--kubeconfig", helpers.HostKubeconfig, "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(yaml)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, out.String())
	}
	return nil
}
