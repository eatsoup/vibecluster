package kubeconfig

import (
	"os"
	"path/filepath"
	"testing"

	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/tools/clientcmd"
)

func testConfig(name string) *clientcmdapi.Config {
	config := clientcmdapi.NewConfig()
	clusterName := "vibecluster-" + name
	config.Clusters[clusterName] = &clientcmdapi.Cluster{
		Server:                   "https://127.0.0.1:6443",
		CertificateAuthorityData: []byte("test-ca"),
	}
	config.AuthInfos[clusterName] = &clientcmdapi.AuthInfo{
		ClientCertificateData: []byte("test-cert"),
		ClientKeyData:         []byte("test-key"),
	}
	config.Contexts[clusterName] = &clientcmdapi.Context{
		Cluster:  clusterName,
		AuthInfo: clusterName,
	}
	config.CurrentContext = clusterName
	return config
}

func TestWriteToFile_NewFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kubeconfig")

	config := testConfig("test")
	if err := WriteToFile(config, path); err != nil {
		t.Fatalf("WriteToFile failed: %v", err)
	}

	// Read back and verify
	loaded, err := clientcmd.LoadFromFile(path)
	if err != nil {
		t.Fatalf("LoadFromFile failed: %v", err)
	}

	if loaded.CurrentContext != "vibecluster-test" {
		t.Errorf("current context = %q, want vibecluster-test", loaded.CurrentContext)
	}
	if _, ok := loaded.Clusters["vibecluster-test"]; !ok {
		t.Error("cluster vibecluster-test not found")
	}
	if _, ok := loaded.AuthInfos["vibecluster-test"]; !ok {
		t.Error("user vibecluster-test not found")
	}
	if _, ok := loaded.Contexts["vibecluster-test"]; !ok {
		t.Error("context vibecluster-test not found")
	}
}

func TestWriteToFile_MergeExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kubeconfig")

	// Write first config
	config1 := testConfig("cluster1")
	if err := WriteToFile(config1, path); err != nil {
		t.Fatalf("first WriteToFile failed: %v", err)
	}

	// Write second config (should merge)
	config2 := testConfig("cluster2")
	if err := WriteToFile(config2, path); err != nil {
		t.Fatalf("second WriteToFile failed: %v", err)
	}

	loaded, err := clientcmd.LoadFromFile(path)
	if err != nil {
		t.Fatalf("LoadFromFile failed: %v", err)
	}

	// Both should exist
	if _, ok := loaded.Clusters["vibecluster-cluster1"]; !ok {
		t.Error("cluster1 was lost during merge")
	}
	if _, ok := loaded.Clusters["vibecluster-cluster2"]; !ok {
		t.Error("cluster2 not found after merge")
	}
	// Current context should be the second one
	if loaded.CurrentContext != "vibecluster-cluster2" {
		t.Errorf("current context = %q, want vibecluster-cluster2", loaded.CurrentContext)
	}
}

func TestWriteToFile_CreatesDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subdir", "kubeconfig")

	config := testConfig("test")
	if err := WriteToFile(config, path); err != nil {
		t.Fatalf("WriteToFile failed: %v", err)
	}

	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Error("file was not created")
	}
}

func TestRemoveFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kubeconfig")

	// Write two configs
	config1 := testConfig("keep")
	if err := WriteToFile(config1, path); err != nil {
		t.Fatalf("WriteToFile 1 failed: %v", err)
	}
	config2 := testConfig("remove")
	if err := WriteToFile(config2, path); err != nil {
		t.Fatalf("WriteToFile 2 failed: %v", err)
	}

	// Remove one
	if err := RemoveFromFile("remove", path); err != nil {
		t.Fatalf("RemoveFromFile failed: %v", err)
	}

	loaded, err := clientcmd.LoadFromFile(path)
	if err != nil {
		t.Fatalf("LoadFromFile failed: %v", err)
	}

	// "keep" should remain
	if _, ok := loaded.Clusters["vibecluster-keep"]; !ok {
		t.Error("vibecluster-keep should still exist")
	}
	if _, ok := loaded.AuthInfos["vibecluster-keep"]; !ok {
		t.Error("vibecluster-keep user should still exist")
	}

	// "remove" should be gone
	if _, ok := loaded.Clusters["vibecluster-remove"]; ok {
		t.Error("vibecluster-remove cluster should have been removed")
	}
	if _, ok := loaded.AuthInfos["vibecluster-remove"]; ok {
		t.Error("vibecluster-remove user should have been removed")
	}
	if _, ok := loaded.Contexts["vibecluster-remove"]; ok {
		t.Error("vibecluster-remove context should have been removed")
	}
}

func TestRemoveFromFile_ClearsCurrentContext(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kubeconfig")

	config := testConfig("active")
	if err := WriteToFile(config, path); err != nil {
		t.Fatalf("WriteToFile failed: %v", err)
	}

	if err := RemoveFromFile("active", path); err != nil {
		t.Fatalf("RemoveFromFile failed: %v", err)
	}

	loaded, err := clientcmd.LoadFromFile(path)
	if err != nil {
		t.Fatalf("LoadFromFile failed: %v", err)
	}

	if loaded.CurrentContext != "" {
		t.Errorf("current context should be empty, got %q", loaded.CurrentContext)
	}
}

func TestRemoveFromFile_NonExistentFile(t *testing.T) {
	// Should not error
	err := RemoveFromFile("test", "/nonexistent/path/kubeconfig")
	if err != nil {
		t.Errorf("RemoveFromFile should not error for non-existent file: %v", err)
	}
}

func TestPrint(t *testing.T) {
	config := testConfig("printtest")

	// Capture stdout
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := Print(config)

	w.Close()
	os.Stdout = old

	if err != nil {
		t.Fatalf("Print failed: %v", err)
	}

	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	output := string(buf[:n])

	if len(output) == 0 {
		t.Error("Print produced no output")
	}

	// Should be valid YAML
	_, err = clientcmd.Load([]byte(output))
	if err != nil {
		t.Errorf("Print output is not valid kubeconfig: %v", err)
	}
}

func TestDefaultKubeconfigPath(t *testing.T) {
	// Test with KUBECONFIG env
	t.Setenv("KUBECONFIG", "/custom/path")
	got := defaultKubeconfigPath()
	if got != "/custom/path" {
		t.Errorf("with KUBECONFIG env, got %q, want /custom/path", got)
	}

	// Test with multiple paths (should use first)
	t.Setenv("KUBECONFIG", "/first/path:/second/path")
	got = defaultKubeconfigPath()
	if got != "/first/path" {
		t.Errorf("with multiple KUBECONFIG paths, got %q, want /first/path", got)
	}

	// Test default
	t.Setenv("KUBECONFIG", "")
	got = defaultKubeconfigPath()
	home, _ := os.UserHomeDir()
	expected := filepath.Join(home, ".kube", "config")
	if got != expected {
		t.Errorf("default path = %q, want %q", got, expected)
	}
}
