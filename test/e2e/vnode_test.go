//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/eatsoup/vibecluster/test/e2e/helpers"
)

// TestVNodeCreation verifies that `vibecluster create --vnode` starts a
// privileged k3s-agent pod that registers as a real Node inside the virtual
// cluster's API server.
func TestVNodeCreation(t *testing.T) {
	name := helpers.UniqueName("vn")
	ns := "vc-" + name
	defer helpers.DumpDebug(t, ns)

	helpers.MustVibeCluster(t, "create", name,
		"--mode=legacy", "--connect=false",
		"--vnode")
	t.Cleanup(func() { helpers.RunVibeCluster(t, "delete", name) })

	// The vnode agent pod is named <name>-vnode-0.
	agentPod := name + "-vnode-0"
	helpers.MustWaitFor(t, 5*time.Minute, 5*time.Second, func() error {
		out, err := helpers.Kubectl(t, helpers.HostKubeconfig,
			"get", "pod", agentPod, "-n", ns, "-o", "jsonpath={.status.phase}")
		if err != nil {
			return err
		}
		if strings.TrimSpace(out) != "Running" {
			return fmt.Errorf("vnode agent pod phase: %s", out)
		}
		return nil
	})

	// The vnode agent must appear as a Ready node in the virtual cluster.
	vcKubeconfig := helpers.GetVClusterKubeconfig(t, name)
	helpers.MustWaitFor(t, 3*time.Minute, 5*time.Second, func() error {
		out, err := helpers.Kubectl(t, vcKubeconfig,
			"get", "nodes", "-o",
			"jsonpath={range .items[*]}{.metadata.name}{\" \"}{.status.conditions[-1].type}{\" \"}{.status.conditions[-1].status}{\"\\n\"}{end}")
		if err != nil {
			return err
		}
		for _, line := range strings.Split(out, "\n") {
			parts := strings.Fields(line)
			// Looking for a node whose name contains "vnode" and is Ready=True.
			if len(parts) == 3 && strings.Contains(parts[0], "vnode") &&
				parts[1] == "Ready" && parts[2] == "True" {
				return nil
			}
		}
		return fmt.Errorf("no Ready vnode node found; nodes:\n%s", out)
	})
}

// TestVNodeMultiNode verifies that `--vnode --nodes N` starts N agent pods and
// all register as Ready nodes in the virtual cluster.
func TestVNodeMultiNode(t *testing.T) {
	name := helpers.UniqueName("vnm")
	ns := "vc-" + name
	defer helpers.DumpDebug(t, ns)

	const nodeCount = 2

	helpers.MustVibeCluster(t, "create", name,
		"--mode=legacy", "--connect=false",
		"--vnode", fmt.Sprintf("--nodes=%d", nodeCount))
	t.Cleanup(func() { helpers.RunVibeCluster(t, "delete", name) })

	// All N agent pods must be Running.
	for i := range nodeCount {
		agentPod := fmt.Sprintf("%s-vnode-%d", name, i)
		helpers.MustWaitFor(t, 8*time.Minute, 5*time.Second, func() error {
			out, err := helpers.Kubectl(t, helpers.HostKubeconfig,
				"get", "pod", agentPod, "-n", ns, "-o", "jsonpath={.status.phase}")
			if err != nil {
				return fmt.Errorf("pod %s: %w", agentPod, err)
			}
			if strings.TrimSpace(out) != "Running" {
				return fmt.Errorf("vnode agent pod %s phase: %s", agentPod, out)
			}
			return nil
		})
	}

	// All N nodes must appear as Ready in the virtual cluster.
	vcKubeconfig := helpers.GetVClusterKubeconfig(t, name)
	helpers.MustWaitFor(t, 5*time.Minute, 5*time.Second, func() error {
		out, err := helpers.Kubectl(t, vcKubeconfig, "get", "nodes",
			"-l", "kubernetes.io/role=agent",
			"-o", "jsonpath={.items[*].status.conditions[-1].status}")
		if err != nil {
			return err
		}
		readyCount := 0
		for _, s := range strings.Fields(out) {
			if s == "True" {
				readyCount++
			}
		}
		if readyCount < nodeCount {
			return fmt.Errorf("want %d Ready agent nodes, got %d; statuses: %s",
				nodeCount, readyCount, out)
		}
		return nil
	})
}

// TestVNodeNetworkPolicy verifies that a default-deny NetworkPolicy inside a
// vnode virtual cluster is actually enforced (in flat-syncer mode, CNI is
// absent so NetworkPolicy is silently ignored).
func TestVNodeNetworkPolicy(t *testing.T) {
	name := helpers.UniqueName("vnp")
	ns := "vc-" + name
	defer helpers.DumpDebug(t, ns)

	helpers.MustVibeCluster(t, "create", name,
		"--mode=legacy", "--connect=false",
		"--vnode")
	t.Cleanup(func() { helpers.RunVibeCluster(t, "delete", name) })

	vcKubeconfig := helpers.GetVClusterKubeconfig(t, name)

	// Deploy a web server.
	helpers.MustKubectl(t, vcKubeconfig,
		"create", "deployment", "web", "--image=nginx:alpine")
	helpers.MustKubectl(t, vcKubeconfig,
		"expose", "deployment", "web", "--port=80", "--name=web")

	// Deploy a client pod.
	helpers.MustKubectl(t, vcKubeconfig,
		"run", "client", "--image=curlimages/curl:latest",
		"--restart=Never", "--command", "--", "sleep", "3600")

	helpers.WaitForPodRunning(t, vcKubeconfig, "default", "app=web", 5*time.Minute)
	helpers.MustWaitFor(t, 3*time.Minute, 5*time.Second, func() error {
		out, _ := helpers.Kubectl(t, vcKubeconfig,
			"get", "pod", "client", "-o", "jsonpath={.status.phase}")
		if strings.TrimSpace(out) == "Running" {
			return nil
		}
		return fmt.Errorf("client pod phase: %s", out)
	})

	// Verify connectivity before policy.
	helpers.MustWaitFor(t, 30*time.Second, 3*time.Second, func() error {
		_, err := helpers.KubectlExec(t, vcKubeconfig, "default", "client", "client",
			"curl", "-sf", "--max-time", "3", "http://web/")
		return err
	})

	// Apply a default-deny-ingress NetworkPolicy.
	helpers.MustKubectlApply(t, vcKubeconfig, `
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: default-deny
  namespace: default
spec:
  podSelector: {}
  policyTypes:
  - Ingress
`)

	// After the policy, curl must fail.
	helpers.MustWaitFor(t, 30*time.Second, 3*time.Second, func() error {
		_, err := helpers.KubectlExec(t, vcKubeconfig, "default", "client", "client",
			"curl", "-sf", "--max-time", "3", "http://web/")
		if err != nil {
			return nil // blocked — expected
		}
		return fmt.Errorf("NetworkPolicy not enforced: curl succeeded after default-deny")
	})
}
