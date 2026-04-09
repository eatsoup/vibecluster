package k8s

import (
	"testing"
)

func TestNamespaceName(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"simple name", "mycluster", "vc-mycluster"},
		{"with dashes", "my-cluster", "vc-my-cluster"},
		{"single char", "a", "vc-a"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NamespaceName(tt.input)
			if got != tt.expected {
				t.Errorf("NamespaceName(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestLabels(t *testing.T) {
	labels := Labels("test")

	if labels[LabelManagedBy] != LabelManagedByValue {
		t.Errorf("managed-by label = %q, want %q", labels[LabelManagedBy], LabelManagedByValue)
	}
	if labels[LabelVClusterName] != "test" {
		t.Errorf("vcluster-name label = %q, want %q", labels[LabelVClusterName], "test")
	}
	if labels["app"] != "vibecluster" {
		t.Errorf("app label = %q, want %q", labels["app"], "vibecluster")
	}

	// Verify labels for different names don't share state
	labels2 := Labels("other")
	if labels2[LabelVClusterName] != "other" {
		t.Errorf("second Labels call got wrong name: %q", labels2[LabelVClusterName])
	}
	if labels[LabelVClusterName] != "test" {
		t.Errorf("first Labels map was mutated: %q", labels[LabelVClusterName])
	}
}

func TestConstants(t *testing.T) {
	if K3sPort != 6443 {
		t.Errorf("K3sPort = %d, want 6443", K3sPort)
	}
	if ServicePort != 443 {
		t.Errorf("ServicePort = %d, want 443", ServicePort)
	}
	if NamespacePrefix != "vc-" {
		t.Errorf("NamespacePrefix = %q, want %q", NamespacePrefix, "vc-")
	}
}
