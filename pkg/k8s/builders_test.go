package k8s

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
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
