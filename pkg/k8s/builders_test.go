package k8s

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestBuildService_DefaultClusterIP(t *testing.T) {
	opts := DefaultBuilderOptions("svc-default")
	svc := BuildService(opts)
	if svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Errorf("svc type = %v, want ClusterIP", svc.Spec.Type)
	}
}

func TestBuildService_LoadBalancerWhenExposed(t *testing.T) {
	opts := DefaultBuilderOptions("svc-lb")
	opts.ExposeType = "LoadBalancer"
	svc := BuildService(opts)
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		t.Errorf("svc type = %v, want LoadBalancer", svc.Spec.Type)
	}
}

func TestBuildService_IngressKeepsClusterIP(t *testing.T) {
	// Ingress mode fronts a ClusterIP service — the Service itself should
	// stay ClusterIP; only an Ingress object is added separately.
	opts := DefaultBuilderOptions("svc-ing")
	opts.ExposeType = "Ingress"
	opts.ExposeHost = "vc.example.com"
	svc := BuildService(opts)
	if svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Errorf("svc type = %v, want ClusterIP for Ingress mode", svc.Spec.Type)
	}
}

func TestBuildStatefulSet_TLSSANIncludesExposeHost(t *testing.T) {
	opts := DefaultBuilderOptions("vc-san")
	opts.ExposeType = "Ingress"
	opts.ExposeHost = "vc.example.com"
	sts := BuildStatefulSet(opts)
	var k3s *corev1.Container
	for i := range sts.Spec.Template.Spec.Containers {
		if sts.Spec.Template.Spec.Containers[i].Name == "k3s" {
			k3s = &sts.Spec.Template.Spec.Containers[i]
		}
	}
	if k3s == nil {
		t.Fatal("k3s container not found")
	}
	want := "--tls-san=vc.example.com"
	found := false
	for _, a := range k3s.Args {
		if a == want {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("k3s args missing %q; got %v", want, k3s.Args)
	}
}

func TestResourceLimits_IsEmpty(t *testing.T) {
	cases := []struct {
		name string
		in   *ResourceLimits
		want bool
	}{
		{"nil", nil, true},
		{"all-empty", &ResourceLimits{}, true},
		{"cpu-only", &ResourceLimits{CPU: "1"}, false},
		{"memory-only", &ResourceLimits{Memory: "1Gi"}, false},
		{"storage-only", &ResourceLimits{Storage: "10Gi"}, false},
		{"pods-only", &ResourceLimits{Pods: 10}, false},
	}
	for _, tc := range cases {
		if got := tc.in.IsEmpty(); got != tc.want {
			t.Errorf("%s: IsEmpty() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestBuildResourceQuota_NilWhenNoLimits(t *testing.T) {
	opts := DefaultBuilderOptions("noquota")
	if rq := BuildResourceQuota(opts); rq != nil {
		t.Errorf("expected nil quota when no limits set, got %+v", rq)
	}
	if lr := BuildLimitRange(opts); lr != nil {
		t.Errorf("expected nil LimitRange when no limits set, got %+v", lr)
	}
}

func TestBuildResourceQuota_AllFields(t *testing.T) {
	opts := DefaultBuilderOptions("rq-all")
	opts.Resources = &ResourceLimits{
		CPU:     "4",
		Memory:  "8Gi",
		Storage: "50Gi",
		Pods:    25,
	}
	rq := BuildResourceQuota(opts)
	if rq == nil {
		t.Fatal("BuildResourceQuota returned nil for non-empty limits")
	}
	if rq.Namespace != opts.Namespace {
		t.Errorf("namespace = %q, want %q", rq.Namespace, opts.Namespace)
	}
	if rq.Name != ResourceQuotaName("rq-all") {
		t.Errorf("name = %q, want %q", rq.Name, ResourceQuotaName("rq-all"))
	}

	cpu := resource.MustParse("4")
	mem := resource.MustParse("8Gi")
	storage := resource.MustParse("50Gi")

	checks := map[corev1.ResourceName]resource.Quantity{
		corev1.ResourceRequestsCPU:     cpu,
		corev1.ResourceLimitsCPU:       cpu,
		corev1.ResourceRequestsMemory:  mem,
		corev1.ResourceLimitsMemory:    mem,
		corev1.ResourceRequestsStorage: storage,
	}
	for k, want := range checks {
		got, ok := rq.Spec.Hard[k]
		if !ok {
			t.Errorf("hard.%s missing", k)
			continue
		}
		if got.Cmp(want) != 0 {
			t.Errorf("hard.%s = %s, want %s", k, got.String(), want.String())
		}
	}
	pods, ok := rq.Spec.Hard[corev1.ResourcePods]
	if !ok {
		t.Fatalf("hard.pods missing")
	}
	if pods.Value() != 25 {
		t.Errorf("hard.pods = %d, want 25", pods.Value())
	}
}

func TestBuildResourceQuota_PartialFields(t *testing.T) {
	// Only memory + pods set; CPU and storage must be absent from the quota,
	// not present-with-zero (which would block all CPU/storage usage).
	opts := DefaultBuilderOptions("rq-partial")
	opts.Resources = &ResourceLimits{
		Memory: "2Gi",
		Pods:   10,
	}
	rq := BuildResourceQuota(opts)
	if rq == nil {
		t.Fatal("BuildResourceQuota returned nil")
	}
	if _, ok := rq.Spec.Hard[corev1.ResourceRequestsCPU]; ok {
		t.Error("requests.cpu should not be set when CPU is empty")
	}
	if _, ok := rq.Spec.Hard[corev1.ResourceRequestsStorage]; ok {
		t.Error("requests.storage should not be set when Storage is empty")
	}
	if _, ok := rq.Spec.Hard[corev1.ResourceRequestsMemory]; !ok {
		t.Error("requests.memory should be set")
	}
}

func TestBuildLimitRange_HasDefaultsForContainer(t *testing.T) {
	// The LimitRange exists so workloads without explicit requests/limits
	// don't get rejected by the matching ResourceQuota at admission time.
	opts := DefaultBuilderOptions("lr-defaults")
	opts.Resources = &ResourceLimits{CPU: "1"}
	lr := BuildLimitRange(opts)
	if lr == nil {
		t.Fatal("BuildLimitRange returned nil for non-empty limits")
	}
	if len(lr.Spec.Limits) != 1 {
		t.Fatalf("expected exactly one LimitRangeItem, got %d", len(lr.Spec.Limits))
	}
	item := lr.Spec.Limits[0]
	if item.Type != corev1.LimitTypeContainer {
		t.Errorf("limit type = %q, want Container", item.Type)
	}
	if _, ok := item.Default[corev1.ResourceCPU]; !ok {
		t.Errorf("default.cpu missing")
	}
	if _, ok := item.DefaultRequest[corev1.ResourceMemory]; !ok {
		t.Errorf("defaultRequest.memory missing")
	}
}

func TestBuildVNodeStatefulSet_DefaultReplicasIsOne(t *testing.T) {
	opts := DefaultBuilderOptions("vc-vn1")
	opts.VNode = true
	sts := BuildVNodeStatefulSet(opts)
	if sts.Spec.Replicas == nil || *sts.Spec.Replicas != 1 {
		t.Errorf("Replicas = %v, want 1 (default when Nodes unset)", sts.Spec.Replicas)
	}
	if sts.Spec.ServiceName != "vc-vn1-vnode" {
		t.Errorf("ServiceName = %q, want vc-vn1-vnode", sts.Spec.ServiceName)
	}
}

func TestBuildVNodeStatefulSet_HonorsNodesCount(t *testing.T) {
	opts := DefaultBuilderOptions("vc-vn3")
	opts.VNode = true
	opts.Nodes = 3
	sts := BuildVNodeStatefulSet(opts)
	if sts.Spec.Replicas == nil || *sts.Spec.Replicas != 3 {
		t.Fatalf("Replicas = %v, want 3", sts.Spec.Replicas)
	}
	// Parallel so 3 privileged agent boots don't serialize.
	if sts.Spec.PodManagementPolicy != "Parallel" {
		t.Errorf("PodManagementPolicy = %q, want Parallel", sts.Spec.PodManagementPolicy)
	}
	// Selector labels must not carry `app: vibecluster` — that would
	// leak vnode pods into the API server Service's backend set.
	if v, ok := sts.Spec.Selector.MatchLabels["app"]; ok {
		t.Errorf("selector includes app=%q — vnode pods must NOT be selected by the main Service", v)
	}
}

func TestBuildVNodeStatefulSet_ZeroNodesTreatedAsOne(t *testing.T) {
	// Operator + legacy paths both send Nodes=0 when the user didn't set
	// the field. The builder must default, not produce a 0-replica STS.
	opts := DefaultBuilderOptions("vc-vnzero")
	opts.VNode = true
	opts.Nodes = 0
	sts := BuildVNodeStatefulSet(opts)
	if sts.Spec.Replicas == nil || *sts.Spec.Replicas != 1 {
		t.Errorf("Replicas = %v, want 1 when Nodes=0", sts.Spec.Replicas)
	}
}

func TestBuildVNodeHeadlessService_SelectsOnlyVNodePods(t *testing.T) {
	opts := DefaultBuilderOptions("vc-vnhs")
	svc := BuildVNodeHeadlessService(opts)
	if svc.Spec.ClusterIP != "None" {
		t.Errorf("ClusterIP = %q, want None (headless)", svc.Spec.ClusterIP)
	}
	if got := svc.Spec.Selector["vibecluster.dev/component"]; got != "vnode" {
		t.Errorf("selector[component] = %q, want vnode", got)
	}
	if _, ok := svc.Spec.Selector["app"]; ok {
		t.Error("vnode headless service must not select `app` — would collide with main API server Service selector")
	}
}

func TestBuildStatefulSet_NoExposeHostNoExtraSAN(t *testing.T) {
	opts := DefaultBuilderOptions("vc-nosan")
	sts := BuildStatefulSet(opts)
	for _, c := range sts.Spec.Template.Spec.Containers {
		if c.Name != "k3s" {
			continue
		}
		for _, a := range c.Args {
			if strings.HasPrefix(a, "--tls-san=") &&
				!strings.HasSuffix(a, ".svc.cluster.local") &&
				!strings.HasSuffix(a, ".svc") &&
				a != "--tls-san=vc-nosan" {
				t.Errorf("unexpected extra TLS-SAN arg: %q", a)
			}
		}
	}
}
