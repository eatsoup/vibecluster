package cli

import (
	"bytes"
	"testing"
)

func TestRootCommand(t *testing.T) {
	cmd := NewRootCommand()

	if cmd.Use != "vibecluster" {
		t.Errorf("root command Use = %q, want vibecluster", cmd.Use)
	}

	// Check all subcommands exist
	expected := map[string]bool{
		"create":   false,
		"delete":   false,
		"list":     false,
		"connect":  false,
		"logs":     false,
		"operator": false,
	}

	for _, sub := range cmd.Commands() {
		if _, ok := expected[sub.Name()]; ok {
			expected[sub.Name()] = true
		}
	}

	for name, found := range expected {
		if !found {
			t.Errorf("missing subcommand %q", name)
		}
	}
}

func TestCreateCommand_Flags(t *testing.T) {
	cmd := NewRootCommand()
	create, _, err := cmd.Find([]string{"create"})
	if err != nil {
		t.Fatalf("create command not found: %v", err)
	}

	flags := []struct {
		name         string
		defaultValue string
	}{
		{"connect", "true"},
		{"timeout", "5m0s"},
		{"print", "false"},
		{"syncer-image", ""},
		{"image-pull-secret", ""},
	}

	for _, f := range flags {
		flag := create.Flags().Lookup(f.name)
		if flag == nil {
			t.Errorf("flag --%s not found", f.name)
			continue
		}
		if flag.DefValue != f.defaultValue {
			t.Errorf("flag --%s default = %q, want %q", f.name, flag.DefValue, f.defaultValue)
		}
	}
}

func TestConnectCommand_Flags(t *testing.T) {
	cmd := NewRootCommand()
	connect, _, err := cmd.Find([]string{"connect"})
	if err != nil {
		t.Fatalf("connect command not found: %v", err)
	}

	for _, name := range []string{"server", "print", "kubeconfig"} {
		if connect.Flags().Lookup(name) == nil {
			t.Errorf("flag --%s not found", name)
		}
	}
}

func TestLogsCommand_Flags(t *testing.T) {
	cmd := NewRootCommand()
	logs, _, err := cmd.Find([]string{"logs"})
	if err != nil {
		t.Fatalf("logs command not found: %v", err)
	}

	followFlag := logs.Flags().Lookup("follow")
	if followFlag == nil {
		t.Fatal("flag --follow not found")
	}
	if followFlag.Shorthand != "f" {
		t.Errorf("follow shorthand = %q, want f", followFlag.Shorthand)
	}

	containerFlag := logs.Flags().Lookup("container")
	if containerFlag == nil {
		t.Fatal("flag --container not found")
	}
	if containerFlag.DefValue != "syncer" {
		t.Errorf("container default = %q, want syncer", containerFlag.DefValue)
	}
}

func TestListCommand_Aliases(t *testing.T) {
	cmd := NewRootCommand()
	list, _, err := cmd.Find([]string{"list"})
	if err != nil {
		t.Fatalf("list command not found: %v", err)
	}

	hasAlias := false
	for _, a := range list.Aliases {
		if a == "ls" {
			hasAlias = true
		}
	}
	if !hasAlias {
		t.Error("list command should have 'ls' alias")
	}
}

func TestGlobalContextFlag(t *testing.T) {
	cmd := NewRootCommand()
	flag := cmd.PersistentFlags().Lookup("context")
	if flag == nil {
		t.Fatal("--context flag not found")
	}
	if flag.DefValue != "" {
		t.Errorf("context default = %q, want empty", flag.DefValue)
	}
}

func TestCreateCommand_RequiresName(t *testing.T) {
	cmd := NewRootCommand()
	cmd.SetArgs([]string{"create"})

	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	err := cmd.Execute()
	if err == nil {
		t.Error("create without name should fail")
	}
}

func TestDeleteCommand_RequiresName(t *testing.T) {
	cmd := NewRootCommand()
	cmd.SetArgs([]string{"delete"})

	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	err := cmd.Execute()
	if err == nil {
		t.Error("delete without name should fail")
	}
}

func TestConnectCommand_RequiresName(t *testing.T) {
	cmd := NewRootCommand()
	cmd.SetArgs([]string{"connect"})

	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	err := cmd.Execute()
	if err == nil {
		t.Error("connect without name should fail")
	}
}

func TestLogsCommand_RequiresName(t *testing.T) {
	cmd := NewRootCommand()
	cmd.SetArgs([]string{"logs"})

	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	err := cmd.Execute()
	if err == nil {
		t.Error("logs without name should fail")
	}
}

func TestOperatorCommand_Structure(t *testing.T) {
	cmd := NewRootCommand()
	operatorCmd, _, err := cmd.Find([]string{"operator"})
	if err != nil {
		t.Fatalf("operator command not found: %v", err)
	}

	expected := map[string]bool{
		"install":   false,
		"uninstall": false,
	}

	for _, sub := range operatorCmd.Commands() {
		if _, ok := expected[sub.Name()]; ok {
			expected[sub.Name()] = true
		}
	}

	for name, found := range expected {
		if !found {
			t.Errorf("missing subcommand %q in operator", name)
		}
	}
}

func TestExposeCommand_Flags(t *testing.T) {
	cmd := NewRootCommand()
	expose, _, err := cmd.Find([]string{"expose"})
	if err != nil {
		t.Fatalf("expose command not found: %v", err)
	}

	flags := []struct {
		name         string
		defaultValue string
	}{
		{"type", ""},
		{"ingress-class", ""},
		{"host", ""},
		{"temp", "false"},
		{"kubeconfig", ""},
	}

	for _, f := range flags {
		flag := expose.Flags().Lookup(f.name)
		if flag == nil {
			t.Errorf("flag --%s not found", f.name)
			continue
		}
		if flag.DefValue != f.defaultValue {
			t.Errorf("flag --%s default = %q, want %q", f.name, flag.DefValue, f.defaultValue)
		}
	}
}

func TestExposeValidation_TempAndTypeMutuallyExclusive(t *testing.T) {
	err := runExpose("foo", &exposeOptions{temp: true, exposeType: "LoadBalancer"})
	if err == nil {
		t.Fatal("expected error when both --temp and --type are set")
	}
	if got := err.Error(); got != "--temp and --type are mutually exclusive" {
		t.Errorf("unexpected error: %q", got)
	}
}

func TestExposeValidation_NeitherTempNorType(t *testing.T) {
	err := runExpose("foo", &exposeOptions{})
	if err == nil {
		t.Fatal("expected error when neither --temp nor --type is set")
	}
	if got := err.Error(); got != "either --type or --temp must be specified" {
		t.Errorf("unexpected error: %q", got)
	}
}

func TestOperatorInstallCommand_Flags(t *testing.T) {
	cmd := NewRootCommand()
	installCmd, _, err := cmd.Find([]string{"operator", "install"})
	if err != nil {
		t.Fatalf("operator install command not found: %v", err)
	}

	flag := installCmd.Flags().Lookup("image")
	if flag == nil {
		t.Fatal("flag --image not found")
	}
	if flag.DefValue != "" {
		t.Errorf("image default = %q, want empty", flag.DefValue)
	}
}
