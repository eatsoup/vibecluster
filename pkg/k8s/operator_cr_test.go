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

func TestCreateVirtualClusterCR_PopulatesExpose(t *testing.T) {
	scheme := runtime.NewScheme()
	gvrToListKind := map[schema.GroupVersionResource]string{
		VirtualClusterGVR: "VirtualClusterList",
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, gvrToListKind)

	spec := VirtualClusterCRSpec{
		SyncerImage: "ghcr.io/example/syncer:v1",
		Expose: &VirtualClusterCRExpose{
			Type:         "Ingress",
			Host:         "vc.example.com",
			IngressClass: "nginx",
		},
	}
	if err := createVirtualClusterCRWith(context.Background(), dyn, "ingvc", "default", spec); err != nil {
		t.Fatalf("create CR: %v", err)
	}

	got, err := dyn.Resource(VirtualClusterGVR).Namespace("default").Get(context.Background(), "ingvc", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get CR: %v", err)
	}
	exposeType, _, _ := unstructured.NestedString(got.Object, "spec", "expose", "type")
	if exposeType != "Ingress" {
		t.Errorf("spec.expose.type = %q, want Ingress", exposeType)
	}
	host, _, _ := unstructured.NestedString(got.Object, "spec", "expose", "host")
	if host != "vc.example.com" {
		t.Errorf("spec.expose.host = %q, want vc.example.com", host)
	}
	ic, _, _ := unstructured.NestedString(got.Object, "spec", "expose", "ingressClass")
	if ic != "nginx" {
		t.Errorf("spec.expose.ingressClass = %q, want nginx", ic)
	}
	syncer, _, _ := unstructured.NestedString(got.Object, "spec", "syncerImage")
	if syncer != "ghcr.io/example/syncer:v1" {
		t.Errorf("spec.syncerImage = %q, want ghcr.io/example/syncer:v1", syncer)
	}
}

func TestCreateVirtualClusterCR_PopulatesResources(t *testing.T) {
	scheme := runtime.NewScheme()
	gvrToListKind := map[schema.GroupVersionResource]string{
		VirtualClusterGVR: "VirtualClusterList",
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, gvrToListKind)

	spec := VirtualClusterCRSpec{
		Resources: &ResourceLimits{
			CPU:    "4",
			Memory: "8Gi",
			Pods:   30,
		},
	}
	if err := createVirtualClusterCRWith(context.Background(), dyn, "rqvc", "default", spec); err != nil {
		t.Fatalf("create CR: %v", err)
	}
	got, err := dyn.Resource(VirtualClusterGVR).Namespace("default").Get(context.Background(), "rqvc", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get CR: %v", err)
	}
	cpu, _, _ := unstructured.NestedString(got.Object, "spec", "resources", "cpu")
	if cpu != "4" {
		t.Errorf("spec.resources.cpu = %q, want 4", cpu)
	}
	mem, _, _ := unstructured.NestedString(got.Object, "spec", "resources", "memory")
	if mem != "8Gi" {
		t.Errorf("spec.resources.memory = %q, want 8Gi", mem)
	}
	// Storage was not set; the field should be absent so the CRD-side
	// default (or "no quota for storage") applies.
	if _, found, _ := unstructured.NestedString(got.Object, "spec", "resources", "storage"); found {
		t.Error("spec.resources.storage should be omitted when not set")
	}
	pods, _, _ := unstructured.NestedInt64(got.Object, "spec", "resources", "pods")
	if pods != 30 {
		t.Errorf("spec.resources.pods = %d, want 30", pods)
	}
}

func TestCreateVirtualClusterCR_NoResourcesOmitsField(t *testing.T) {
	scheme := runtime.NewScheme()
	gvrToListKind := map[schema.GroupVersionResource]string{
		VirtualClusterGVR: "VirtualClusterList",
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, gvrToListKind)

	if err := createVirtualClusterCRWith(context.Background(), dyn, "norq", "default", VirtualClusterCRSpec{}); err != nil {
		t.Fatalf("create CR: %v", err)
	}
	got, err := dyn.Resource(VirtualClusterGVR).Namespace("default").Get(context.Background(), "norq", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get CR: %v", err)
	}
	if _, found, _ := unstructured.NestedMap(got.Object, "spec", "resources"); found {
		t.Error("spec.resources should be omitted when no resources configured")
	}
}

func TestCreateVirtualClusterCR_NoExposeOmitsField(t *testing.T) {
	scheme := runtime.NewScheme()
	gvrToListKind := map[schema.GroupVersionResource]string{
		VirtualClusterGVR: "VirtualClusterList",
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, gvrToListKind)

	if err := createVirtualClusterCRWith(context.Background(), dyn, "noexp", "default", VirtualClusterCRSpec{}); err != nil {
		t.Fatalf("create CR: %v", err)
	}
	got, err := dyn.Resource(VirtualClusterGVR).Namespace("default").Get(context.Background(), "noexp", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get CR: %v", err)
	}
	if _, found, _ := unstructured.NestedMap(got.Object, "spec", "expose"); found {
		t.Errorf("spec.expose should be omitted when no expose configured")
	}
}

func TestCreateVirtualClusterCR_NodesOnlySetWhenGreaterThanOne(t *testing.T) {
	// Nodes=0 or Nodes=1 should omit the field so the CRD default applies
	// and two separate clients can't disagree about "default means 1".
	for _, n := range []int32{0, 1} {
		scheme := runtime.NewScheme()
		gvrToListKind := map[schema.GroupVersionResource]string{
			VirtualClusterGVR: "VirtualClusterList",
		}
		dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, gvrToListKind)
		if err := createVirtualClusterCRWith(context.Background(), dyn, "n1", "default", VirtualClusterCRSpec{VNode: true, Nodes: n}); err != nil {
			t.Fatalf("create CR: %v", err)
		}
		got, err := dyn.Resource(VirtualClusterGVR).Namespace("default").Get(context.Background(), "n1", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get CR: %v", err)
		}
		if _, found, _ := unstructured.NestedInt64(got.Object, "spec", "nodes"); found {
			t.Errorf("Nodes=%d: spec.nodes should be omitted (let CRD default apply)", n)
		}
	}
}

func TestCreateVirtualClusterCR_NodesPopulatedWhenMulti(t *testing.T) {
	scheme := runtime.NewScheme()
	gvrToListKind := map[schema.GroupVersionResource]string{
		VirtualClusterGVR: "VirtualClusterList",
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, gvrToListKind)
	if err := createVirtualClusterCRWith(context.Background(), dyn, "multi", "default", VirtualClusterCRSpec{VNode: true, Nodes: 3}); err != nil {
		t.Fatalf("create CR: %v", err)
	}
	got, err := dyn.Resource(VirtualClusterGVR).Namespace("default").Get(context.Background(), "multi", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get CR: %v", err)
	}
	n, found, _ := unstructured.NestedInt64(got.Object, "spec", "nodes")
	if !found {
		t.Fatal("spec.nodes missing")
	}
	if n != 3 {
		t.Errorf("spec.nodes = %d, want 3", n)
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
