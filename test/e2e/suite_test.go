//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eatsoup/vibecluster/test/e2e/helpers"
)

const k3dClusterName = "vibecluster-e2e"

func TestMain(m *testing.M) {
	if err := setup(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e setup failed: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	teardown()
	os.Exit(code)
}

func setup() error {
	bin := os.Getenv("VIBC_BIN")
	if bin == "" {
		wd, _ := os.Getwd()
		root := wd
		for {
			if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
				break
			}
			parent := filepath.Dir(root)
			if parent == root {
				return fmt.Errorf("could not find project root; set VIBC_BIN")
			}
			root = parent
		}
		bin = filepath.Join(root, "bin", "vibecluster")
		if _, err := os.Stat(bin); err != nil {
			return fmt.Errorf("binary not found at %s; run `make build` or set VIBC_BIN", bin)
		}
	}
	helpers.VibeclusterBin = bin
	helpers.SyncerImage = os.Getenv("VIBC_SYNCER_IMAGE")
	helpers.OperatorImage = os.Getenv("VIBC_OPERATOR_IMAGE")

	for _, tool := range []string{"k3d", "kubectl"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("%s not found in PATH", tool)
		}
	}

	if existing := os.Getenv("VIBC_HOST_KUBECONFIG"); existing != "" {
		helpers.HostKubeconfig = existing
	} else {
		if err := createK3dCluster(); err != nil {
			return err
		}
	}

	return createSharedVCluster()
}

func createK3dCluster() error {
	fmt.Println("Creating k3d cluster", k3dClusterName, "...")
	if err := runCmd("k3d", "cluster", "create", k3dClusterName,
		"--agents", "2",
		"--wait",
		"--timeout", "5m",
		"--k3s-arg", "--disable=traefik@server:*",
	); err != nil {
		return fmt.Errorf("k3d cluster create: %w", err)
	}

	kcPath := filepath.Join(os.TempDir(), "vibecluster-e2e-host.kubeconfig")
	out, err := runCmdOutput("k3d", "kubeconfig", "get", k3dClusterName)
	if err != nil {
		return fmt.Errorf("k3d kubeconfig get: %w", err)
	}
	if err := os.WriteFile(kcPath, []byte(out), 0600); err != nil {
		return fmt.Errorf("writing host kubeconfig: %w", err)
	}
	helpers.HostKubeconfig = kcPath

	// Import images so nodes don't pull from a registry.
	// Always import when set — e2e images are local-only.
	if img := helpers.SyncerImage; img != "" {
		fmt.Println("Loading syncer image into k3d...")
		if err := runCmd("k3d", "image", "import", img, "-c", k3dClusterName); err != nil {
			return fmt.Errorf("loading syncer image: %w", err)
		}
	}
	if img := helpers.OperatorImage; img != "" {
		fmt.Println("Loading operator image into k3d...")
		if err := runCmd("k3d", "image", "import", img, "-c", k3dClusterName); err != nil {
			return fmt.Errorf("loading operator image: %w", err)
		}
	}
	return nil
}

func createSharedVCluster() error {
	name := "shared"
	helpers.SharedVCName = name

	fmt.Printf("Creating shared vcluster %q ...\n", name)
	args := []string{"create", name, "--mode=legacy", "--connect=false"}
	if helpers.SyncerImage != "" {
		args = append(args, "--syncer-image", helpers.SyncerImage)
	}
	cmd := exec.Command(helpers.VibeclusterBin, args...)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+helpers.HostKubeconfig)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("creating shared vcluster: %w", err)
	}

	kcPath, err := helpers.SetupSharedVCluster(name)
	if err != nil {
		return fmt.Errorf("waiting for shared vcluster: %w", err)
	}
	helpers.SharedVCKubeconfig = kcPath
	fmt.Println("Shared vcluster ready.")
	return nil
}

func teardown() {
	if helpers.SharedVCName != "" {
		cmd := exec.Command(helpers.VibeclusterBin, "delete", helpers.SharedVCName)
		cmd.Env = append(os.Environ(), "KUBECONFIG="+helpers.HostKubeconfig)
		cmd.Run() //nolint:errcheck
	}
	if os.Getenv("VIBC_HOST_KUBECONFIG") != "" {
		return
	}
	if os.Getenv("VIBC_KEEP_CLUSTER") != "" {
		fmt.Println("VIBC_KEEP_CLUSTER set; leaving k3d cluster", k3dClusterName)
		return
	}
	fmt.Println("Deleting k3d cluster", k3dClusterName, "...")
	runCmd("k3d", "cluster", "delete", k3dClusterName) //nolint:errcheck
}

func runCmd(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func runCmdOutput(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return string(out), nil
}

// TestSharedVCluster is a no-op test that exists only to give the test runner a
// concrete test to show when it prints coverage — without it, a `-run` that
// selects no tests produces a warning.
func TestSharedVCluster(t *testing.T) {
	if helpers.SharedVCKubeconfig == "" {
		t.Fatal("shared vcluster kubeconfig not set")
	}
	out, err := helpers.Kubectl(t, helpers.SharedVCKubeconfig, "get", "nodes")
	if err != nil {
		t.Fatalf("shared vcluster unreachable: %v", err)
	}
	t.Logf("shared vcluster nodes:\n%s", out)
}
