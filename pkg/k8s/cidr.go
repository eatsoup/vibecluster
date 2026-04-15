package k8s

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// VNodeCIDRs is the per-vcluster pod/service CIDR pair used in vnode mode.
// ClusterDNS is the first `.0.10` address of the service CIDR — the value
// k3s configures coredns to listen on.
type VNodeCIDRs struct {
	Pod        string
	Service    string
	ClusterDNS string
}

// vnode CIDR pool. Each /16 is chosen to avoid the k3s host defaults
// (10.42/16 for pods, 10.43/16 for services) and the typical service CIDRs
// shipped by EKS/GKE. Ten slots per family is a deliberate prototype ceiling
// — productization would switch to smaller per-vcluster ranges (e.g. /20) to
// fit more clusters on one host.
const (
	vnodePodCIDRStart = 244
	vnodePodCIDREnd   = 253
	vnodeSvcCIDRStart = 96
	vnodeSvcCIDREnd   = 105
)

// AllocateVNodeCIDRs picks a free pod/service CIDR pair for a new vnode
// virtual cluster by inspecting existing vibecluster-managed namespaces for
// their CIDR annotations. The caller is responsible for setting the
// AnnotationPodCIDR / AnnotationServiceCIDR annotations on the new namespace
// so future allocations see this one as used.
func AllocateVNodeCIDRs(ctx context.Context, client kubernetes.Interface) (VNodeCIDRs, error) {
	nss, err := client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{
		LabelSelector: LabelManagedBy + "=" + LabelManagedByValue,
	})
	if err != nil {
		return VNodeCIDRs{}, fmt.Errorf("listing vibecluster namespaces: %w", err)
	}
	return pickFreeVNodeCIDRs(collectUsedVNodeCIDRs(nss.Items))
}

type usedVNodeCIDRs struct {
	pod map[string]struct{}
	svc map[string]struct{}
}

func collectUsedVNodeCIDRs(nss []corev1.Namespace) usedVNodeCIDRs {
	u := usedVNodeCIDRs{
		pod: map[string]struct{}{},
		svc: map[string]struct{}{},
	}
	for _, ns := range nss {
		if v := ns.Annotations[AnnotationPodCIDR]; v != "" {
			u.pod[v] = struct{}{}
		}
		if v := ns.Annotations[AnnotationServiceCIDR]; v != "" {
			u.svc[v] = struct{}{}
		}
	}
	return u
}

func pickFreeVNodeCIDRs(used usedVNodeCIDRs) (VNodeCIDRs, error) {
	var r VNodeCIDRs
	for i := vnodePodCIDRStart; i <= vnodePodCIDREnd; i++ {
		c := fmt.Sprintf("10.%d.0.0/16", i)
		if _, in := used.pod[c]; !in {
			r.Pod = c
			break
		}
	}
	for i := vnodeSvcCIDRStart; i <= vnodeSvcCIDREnd; i++ {
		c := fmt.Sprintf("10.%d.0.0/16", i)
		if _, in := used.svc[c]; !in {
			r.Service = c
			r.ClusterDNS = fmt.Sprintf("10.%d.0.10", i)
			break
		}
	}
	if r.Pod == "" || r.Service == "" {
		return VNodeCIDRs{}, fmt.Errorf("no free vnode CIDR pair in pool (pod 10.%d-%d.0.0/16, service 10.%d-%d.0.0/16); delete an existing vnode vcluster or extend the pool",
			vnodePodCIDRStart, vnodePodCIDREnd, vnodeSvcCIDRStart, vnodeSvcCIDREnd)
	}
	return r, nil
}
