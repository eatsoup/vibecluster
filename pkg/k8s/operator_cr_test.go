package k8s

import (
	"context"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func TestIsOperatorAvailable_CRDInstalled(t *testing.T) {
	ext := apiextensionsfake.NewSimpleClientset(&apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: CRDName},
	})
	got, err := isOperatorAvailableWith(context.Background(), ext)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got {
		t.Error("expected available=true when CRD exists")
	}
}

func TestIsOperatorAvailable_CRDMissing(t *testing.T) {
	ext := apiextensionsfake.NewSimpleClientset()
	got, err := isOperatorAvailableWith(context.Background(), ext)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got {
		t.Error("expected available=false when CRD missing")
	}
}

// newDynamicFake builds a fake dynamic client preloaded with VirtualCluster CRs.
func newDynamicFake(t *testing.T, items ...*unstructured.Unstructured) *dynamicfake.FakeDynamicClient {
	t.Helper()
	scheme := runtime.NewScheme()
	gvrToListKind := map[schema.GroupVersionResource]string{
		VirtualClusterGVR: "VirtualClusterList",
	}
	objs := make([]runtime.Object, 0, len(items))
	for _, it := range items {
		objs = append(objs, it)
	}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, gvrToListKind, objs...)
}

func newVCCR(name, namespace, phase string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "vibecluster.dev/v1alpha1",
			"kind":       "VirtualCluster",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": namespace,
			},
		},
	}
	if phase != "" {
		obj.Object["status"] = map[string]interface{}{"phase": phase}
	}
	return obj
}

func TestFindVirtualClusterCR_Found(t *testing.T) {
	dyn := newDynamicFake(t,
		newVCCR("alpha", "default", "Ready"),
		newVCCR("beta", "team-a", "Pending"),
	)
	ns, err := findVirtualClusterCRWith(context.Background(), dyn, "beta")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ns != "team-a" {
		t.Errorf("namespace = %q, want team-a", ns)
	}
}

func TestFindVirtualClusterCR_NotFound(t *testing.T) {
	dyn := newDynamicFake(t, newVCCR("alpha", "default", ""))
	ns, err := findVirtualClusterCRWith(context.Background(), dyn, "missing")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ns != "" {
		t.Errorf("namespace = %q, want empty", ns)
	}
}

func TestListVirtualClusterCRs(t *testing.T) {
	dyn := newDynamicFake(t,
		newVCCR("alpha", "default", "Ready"),
		newVCCR("beta", "team-a", "Pending"),
		newVCCR("gamma", "team-b", ""),
	)
	got, err := listVirtualClusterCRsWith(context.Background(), dyn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 CRs, got %d", len(got))
	}
	byName := map[string]VClusterCR{}
	for _, c := range got {
		byName[c.Name] = c
	}
	if byName["alpha"].Phase != "Ready" {
		t.Errorf("alpha.Phase = %q, want Ready", byName["alpha"].Phase)
	}
	if byName["beta"].Namespace != "team-a" {
		t.Errorf("beta.Namespace = %q, want team-a", byName["beta"].Namespace)
	}
	if byName["gamma"].Phase != "" {
		t.Errorf("gamma.Phase = %q, want empty (no status)", byName["gamma"].Phase)
	}
}

func TestListVirtualClusterCRs_Empty(t *testing.T) {
	dyn := newDynamicFake(t)
	got, err := listVirtualClusterCRsWith(context.Background(), dyn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 CRs, got %d", len(got))
	}
}
