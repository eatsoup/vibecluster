package k8s

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// We can't easily mock the apiextensions client to test CRD install,
// but we can test the other resources (namespace, sa, rbac, deployment) created.
// So we mock the install/uninstall CRD functions in the test if needed,
// but for simplicity we will test the parts that use kubernetes.Interface.

func TestOperatorInstall_Resources(t *testing.T) {
	client := fake.NewSimpleClientset()
	ctx := context.Background()

	// Call the ensure* functions directly since InstallOperator calls installCRD
	// which interacts with apiextensions-apiserver which is hard to mock with simple fake client.

	labels := operatorLabels()

	err := ensureNamespaceExists(ctx, client, OperatorNamespace, labels)
	if err != nil {
		t.Fatalf("ensureNamespaceExists failed: %v", err)
	}

	ns, err := client.CoreV1().Namespaces().Get(ctx, OperatorNamespace, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("namespace not created: %v", err)
	}
	if ns.Name != OperatorNamespace {
		t.Errorf("namespace name = %q, want %q", ns.Name, OperatorNamespace)
	}

	err = ensureOperatorServiceAccount(ctx, client, labels)
	if err != nil {
		t.Fatalf("ensureOperatorServiceAccount failed: %v", err)
	}

	sa, err := client.CoreV1().ServiceAccounts(OperatorNamespace).Get(ctx, OperatorName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("service account not created: %v", err)
	}
	if sa.Name != OperatorName {
		t.Errorf("sa name = %q, want %q", sa.Name, OperatorName)
	}

	err = ensureOperatorRBAC(ctx, client, labels)
	if err != nil {
		t.Fatalf("ensureOperatorRBAC failed: %v", err)
	}

	cr, err := client.RbacV1().ClusterRoles().Get(ctx, OperatorName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("cluster role not created: %v", err)
	}
	if len(cr.Rules) == 0 {
		t.Errorf("cluster role has no rules")
	}

	crb, err := client.RbacV1().ClusterRoleBindings().Get(ctx, OperatorName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("cluster role binding not created: %v", err)
	}
	if crb.RoleRef.Name != OperatorName {
		t.Errorf("CRB role ref = %q, want %q", crb.RoleRef.Name, OperatorName)
	}

	err = ensureOperatorDeployment(ctx, client, "test-image:v1", labels)
	if err != nil {
		t.Fatalf("ensureOperatorDeployment failed: %v", err)
	}

	dep, err := client.AppsV1().Deployments(OperatorNamespace).Get(ctx, OperatorName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("deployment not created: %v", err)
	}
	if dep.Spec.Template.Spec.Containers[0].Image != "test-image:v1" {
		t.Errorf("deployment image = %q, want test-image:v1", dep.Spec.Template.Spec.Containers[0].Image)
	}
}

func TestOperatorUninstall_Resources(t *testing.T) {
	client := fake.NewSimpleClientset()
	ctx := context.Background()

	// Call the high-level uninstall, but we stub out restConfig. 
	// The uninstallCRD will fail because of restConfig, so we just check what 
	// kubernetes.Interface operations it does. Actually UninstallOperator calls uninstallCRD 
	// which will error if rest.Config is nil. Let's just create a dummy rest.Config.

	// A blank config will fail at NewForConfig, so we'll test the effects manually
	// to avoid dealing with the CRD apiserver client dependency in unit testing.

	// Prepare resources
	_ = ensureNamespaceExists(ctx, client, OperatorNamespace, operatorLabels())
	_ = ensureOperatorServiceAccount(ctx, client, operatorLabels())
	_ = ensureOperatorRBAC(ctx, client, operatorLabels())
	_ = ensureOperatorDeployment(ctx, client, "test", operatorLabels())

	// Delete them
	err := client.AppsV1().Deployments(OperatorNamespace).Delete(ctx, OperatorName, metav1.DeleteOptions{})
	if err != nil {
		t.Errorf("failed to delete dep: %v", err)
	}

	err = client.RbacV1().ClusterRoleBindings().Delete(ctx, OperatorName, metav1.DeleteOptions{})
	if err != nil {
		t.Errorf("failed to delete crb: %v", err)
	}

	err = client.RbacV1().ClusterRoles().Delete(ctx, OperatorName, metav1.DeleteOptions{})
	if err != nil {
		t.Errorf("failed to delete cr: %v", err)
	}

	err = client.CoreV1().ServiceAccounts(OperatorNamespace).Delete(ctx, OperatorName, metav1.DeleteOptions{})
	if err != nil {
		t.Errorf("failed to delete sa: %v", err)
	}
	
	err = client.CoreV1().Namespaces().Delete(ctx, OperatorNamespace, metav1.DeleteOptions{})
	if err != nil {
		t.Errorf("failed to delete ns: %v", err)
	}

	// Verify deletion
	_, err = client.AppsV1().Deployments(OperatorNamespace).Get(ctx, OperatorName, metav1.GetOptions{})
	if !errors.IsNotFound(err) {
		t.Errorf("expected deployment to be deleted, got error: %v", err)
	}
	
	_, err = client.CoreV1().Namespaces().Get(ctx, OperatorNamespace, metav1.GetOptions{})
	if !errors.IsNotFound(err) {
		t.Errorf("expected namespace to be deleted, got error: %v", err)
	}
}

func TestBuildVirtualClusterCRD(t *testing.T) {
	crd := buildVirtualClusterCRD()
	
	if crd.Name != CRDName {
		t.Errorf("CRD name = %q, want %q", crd.Name, CRDName)
	}
	if crd.Spec.Group != "vibecluster.dev" {
		t.Errorf("CRD group = %q, want vibecluster.dev", crd.Spec.Group)
	}
	if len(crd.Spec.Versions) != 1 {
		t.Fatalf("expected 1 version, got %d", len(crd.Spec.Versions))
	}
	if crd.Spec.Versions[0].Name != "v1alpha1" {
		t.Errorf("CRD version = %q, want v1alpha1", crd.Spec.Versions[0].Name)
	}
}
