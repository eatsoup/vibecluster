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
	// Resolve binary path.
	bin := os.Getenv("VIBC_BIN")
	if bin == "" {
		// Try default build output.
		wd, _ := os.Getwd()
		// Walk up to find project root (contains go.mod).
		root := wd
		for {
			if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
				break
			}
			parent := filepath.Dir(root)
			if parent == root {
				return fmt.Errorf("could not find project root (go.mod); set VIBC_BIN")
			}
			root = parent
		}
		bin = filepath.Join(root, "bin", "vibecluster")
		if _, err := os.Stat(bin); err != nil {
			return fmt.Errorf("binary not found at %s; run `make build` or set VIBC_BIN", bin)
		}
	}
	helpers.VibeclusterBin = bin

	// Optional image overrides.
	helpers.SyncerImage = os.Getenv("VIBC_SYNCER_IMAGE")
	helpers.OperatorImage = os.Getenv("VIBC_OPERATOR_IMAGE")

	// Check required tools.
	for _, tool := range []string{"k3d", "kubectl"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("%s not found in PATH", tool)
		}
	}

	// Use an existing cluster if provided, otherwise create one.
	if existing := os.Getenv("VIBC_HOST_KUBECONFIG"); existing != "" {
		helpers.HostKubeconfig = existing
		return nil
	}

	fmt.Println("Creating k3d cluster", k3dClusterName, "...")
	if err := runCmd("k3d", "cluster", "create", k3dClusterName,
		"--agents", "2",
		"--wait",
		"--timeout", "5m",
		"--k3s-arg", "--disable=traefik@server:*",
	); err != nil {
		return fmt.Errorf("k3d cluster create: %w", err)
	}

	// Write kubeconfig to a temp file so we don't pollute the user's default.
	kcPath := filepath.Join(os.TempDir(), "vibecluster-e2e-host.kubeconfig")
	out, err := runCmdOutput("k3d", "kubeconfig", "get", k3dClusterName)
	if err != nil {
		return fmt.Errorf("k3d kubeconfig get: %w", err)
	}
	if err := os.WriteFile(kcPath, []byte(out), 0600); err != nil {
		return fmt.Errorf("writing host kubeconfig: %w", err)
	}
	helpers.HostKubeconfig = kcPath

	// Load custom images if specified.
	if img := helpers.SyncerImage; img != "" && !strings.HasPrefix(img, "ghcr.io") {
		fmt.Println("Loading syncer image into k3d...")
		if err := runCmd("k3d", "image", "import", img, "-c", k3dClusterName); err != nil {
			return fmt.Errorf("loading syncer image: %w", err)
		}
	}
	if img := helpers.OperatorImage; img != "" && !strings.HasPrefix(img, "ghcr.io") {
		fmt.Println("Loading operator image into k3d...")
		if err := runCmd("k3d", "image", "import", img, "-c", k3dClusterName); err != nil {
			return fmt.Errorf("loading operator image: %w", err)
		}
	}

	return nil
}

func teardown() {
	if os.Getenv("VIBC_HOST_KUBECONFIG") != "" {
		// User-supplied cluster; don't touch it.
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
