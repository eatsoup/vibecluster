package k8s

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestAllocateVNodeCIDRs_EmptyCluster(t *testing.T) {
	client := fake.NewSimpleClientset()
	cidrs, err := AllocateVNodeCIDRs(context.Background(), client)
	if err != nil {
		t.Fatalf("AllocateVNodeCIDRs: %v", err)
	}
	if cidrs.Pod != "10.244.0.0/16" {
		t.Errorf("Pod = %q, want 10.244.0.0/16", cidrs.Pod)
	}
	if cidrs.Service != "10.96.0.0/16" {
		t.Errorf("Service = %q, want 10.96.0.0/16", cidrs.Service)
	}
	if cidrs.ClusterDNS != "10.96.0.10" {
		t.Errorf("ClusterDNS = %q, want 10.96.0.10", cidrs.ClusterDNS)
	}
}

func TestAllocateVNodeCIDRs_SkipsUsed(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "vc-first",
				Labels: Labels("first"),
				Annotations: map[string]string{
					AnnotationPodCIDR:     "10.244.0.0/16",
					AnnotationServiceCIDR: "10.96.0.0/16",
				},
			},
		},
		&corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "vc-second",
				Labels: Labels("second"),
				Annotations: map[string]string{
					AnnotationPodCIDR:     "10.245.0.0/16",
					AnnotationServiceCIDR: "10.97.0.0/16",
				},
			},
		},
	)
	cidrs, err := AllocateVNodeCIDRs(context.Background(), client)
	if err != nil {
		t.Fatalf("AllocateVNodeCIDRs: %v", err)
	}
	if cidrs.Pod != "10.246.0.0/16" {
		t.Errorf("Pod = %q, want 10.246.0.0/16", cidrs.Pod)
	}
	if cidrs.Service != "10.98.0.0/16" {
		t.Errorf("Service = %q, want 10.98.0.0/16", cidrs.Service)
	}
	if cidrs.ClusterDNS != "10.98.0.10" {
		t.Errorf("ClusterDNS = %q, want 10.98.0.10", cidrs.ClusterDNS)
	}
}

func TestAllocateVNodeCIDRs_IgnoresNonVibeclusterNamespaces(t *testing.T) {
	// A namespace not labelled with managed-by=vibecluster should not
	// influence the allocation even if it has matching annotations.
	client := fake.NewSimpleClientset(
		&corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: "some-other-ns",
				Annotations: map[string]string{
					AnnotationPodCIDR:     "10.244.0.0/16",
					AnnotationServiceCIDR: "10.96.0.0/16",
				},
			},
		},
	)
	cidrs, err := AllocateVNodeCIDRs(context.Background(), client)
	if err != nil {
		t.Fatalf("AllocateVNodeCIDRs: %v", err)
	}
	if cidrs.Pod != "10.244.0.0/16" {
		t.Errorf("Pod = %q, want 10.244.0.0/16 (non-vibecluster ns should be ignored)", cidrs.Pod)
	}
}

func TestAllocateVNodeCIDRs_PoolExhausted(t *testing.T) {
	used := usedVNodeCIDRs{
		pod: map[string]struct{}{},
		svc: map[string]struct{}{},
	}
	for i := vnodePodCIDRStart; i <= vnodePodCIDREnd; i++ {
		used.pod[fmtCIDR(i)] = struct{}{}
	}
	for i := vnodeSvcCIDRStart; i <= vnodeSvcCIDREnd; i++ {
		used.svc[fmtCIDR(i)] = struct{}{}
	}
	if _, err := pickFreeVNodeCIDRs(used); err == nil {
		t.Fatal("expected error when pool is exhausted, got nil")
	}
}

func fmtCIDR(octet int) string {
	return "10." + itoa(octet) + ".0.0/16"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [4]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func TestCreateVirtualCluster_VNodeAnnotatesCIDRs(t *testing.T) {
	client := fake.NewSimpleClientset()
	ctx := context.Background()

	if err := CreateVirtualCluster(ctx, client, "vntest", CreateOptions{VNode: true}); err != nil {
		t.Fatalf("CreateVirtualCluster failed: %v", err)
	}

	ns, err := client.CoreV1().Namespaces().Get(ctx, "vc-vntest", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("namespace not created: %v", err)
	}
	if ns.Annotations[AnnotationPodCIDR] != "10.244.0.0/16" {
		t.Errorf("pod-cidr annotation = %q, want 10.244.0.0/16", ns.Annotations[AnnotationPodCIDR])
	}
	if ns.Annotations[AnnotationServiceCIDR] != "10.96.0.0/16" {
		t.Errorf("service-cidr annotation = %q, want 10.96.0.0/16", ns.Annotations[AnnotationServiceCIDR])
	}

	// A second vcluster on the same host must get a different CIDR pair.
	if err := CreateVirtualCluster(ctx, client, "vntest2", CreateOptions{VNode: true}); err != nil {
		t.Fatalf("second CreateVirtualCluster failed: %v", err)
	}
	ns2, err := client.CoreV1().Namespaces().Get(ctx, "vc-vntest2", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("second namespace not created: %v", err)
	}
	if ns2.Annotations[AnnotationPodCIDR] == ns.Annotations[AnnotationPodCIDR] {
		t.Errorf("second vcluster got the same pod CIDR %q as the first — allocator collision", ns2.Annotations[AnnotationPodCIDR])
	}
	if ns2.Annotations[AnnotationServiceCIDR] == ns.Annotations[AnnotationServiceCIDR] {
		t.Errorf("second vcluster got the same service CIDR %q as the first — allocator collision", ns2.Annotations[AnnotationServiceCIDR])
	}
}
