package k8s

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestCreateVirtualCluster(t *testing.T) {
	client := fake.NewSimpleClientset()
	ctx := context.Background()

	err := CreateVirtualCluster(ctx, client, "test", CreateOptions{})
	if err != nil {
		t.Fatalf("CreateVirtualCluster failed: %v", err)
	}

	// Verify namespace
	ns, err := client.CoreV1().Namespaces().Get(ctx, "vc-test", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("namespace not created: %v", err)
	}
	if ns.Labels[LabelManagedBy] != LabelManagedByValue {
		t.Errorf("namespace missing managed-by label")
	}
	if ns.Labels[LabelVClusterName] != "test" {
		t.Errorf("namespace missing vcluster-name label")
	}
	if ns.Annotations[AnnotationCreated] == "" {
		t.Errorf("namespace missing created annotation")
	}

	// Verify service account
	sa, err := client.CoreV1().ServiceAccounts("vc-test").Get(ctx, "vc-test", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("service account not created: %v", err)
	}
	if sa.Labels[LabelManagedBy] != LabelManagedByValue {
		t.Errorf("service account missing managed-by label")
	}

	// Verify ClusterRole
	crName := "vc-test-vc-test"
	cr, err := client.RbacV1().ClusterRoles().Get(ctx, crName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("cluster role not created: %v", err)
	}
	if len(cr.Rules) == 0 {
		t.Errorf("cluster role has no rules")
	}
	// Verify it has pod permissions
	hasPodRule := false
	for _, rule := range cr.Rules {
		for _, res := range rule.Resources {
			if res == "pods" {
				hasPodRule = true
			}
		}
	}
	if !hasPodRule {
		t.Errorf("cluster role missing pods permission")
	}

	// Verify ClusterRoleBinding
	crb, err := client.RbacV1().ClusterRoleBindings().Get(ctx, crName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("cluster role binding not created: %v", err)
	}
	if crb.RoleRef.Name != crName {
		t.Errorf("CRB role ref = %q, want %q", crb.RoleRef.Name, crName)
	}
	if len(crb.Subjects) != 1 || crb.Subjects[0].Name != "vc-test" {
		t.Errorf("CRB subjects unexpected: %+v", crb.Subjects)
	}

	// Verify Role and RoleBinding
	_, err = client.RbacV1().Roles("vc-test").Get(ctx, "vc-test", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("role not created: %v", err)
	}
	_, err = client.RbacV1().RoleBindings("vc-test").Get(ctx, "vc-test", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("role binding not created: %v", err)
	}

	// Verify services
	svc, err := client.CoreV1().Services("vc-test").Get(ctx, "test", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("service not created: %v", err)
	}
	if svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Errorf("service type = %q, want ClusterIP", svc.Spec.Type)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != ServicePort {
		t.Errorf("service port unexpected: %+v", svc.Spec.Ports)
	}

	headlessSvc, err := client.CoreV1().Services("vc-test").Get(ctx, "test-headless", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("headless service not created: %v", err)
	}
	if headlessSvc.Spec.ClusterIP != "None" {
		t.Errorf("headless service ClusterIP = %q, want None", headlessSvc.Spec.ClusterIP)
	}

	// Verify StatefulSet
	sts, err := client.AppsV1().StatefulSets("vc-test").Get(ctx, "test", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("statefulset not created: %v", err)
	}
	if *sts.Spec.Replicas != 1 {
		t.Errorf("replicas = %d, want 1", *sts.Spec.Replicas)
	}
	if sts.Spec.ServiceName != "test-headless" {
		t.Errorf("serviceName = %q, want test-headless", sts.Spec.ServiceName)
	}

	// Verify containers
	containers := sts.Spec.Template.Spec.Containers
	if len(containers) != 2 {
		t.Fatalf("expected 2 containers, got %d", len(containers))
	}

	k3sCont := containers[0]
	if k3sCont.Name != "k3s" {
		t.Errorf("first container name = %q, want k3s", k3sCont.Name)
	}
	if k3sCont.Image != K3sImage {
		t.Errorf("k3s image = %q, want %q", k3sCont.Image, K3sImage)
	}

	syncerCont := containers[1]
	if syncerCont.Name != "syncer" {
		t.Errorf("second container name = %q, want syncer", syncerCont.Name)
	}
	if syncerCont.Image != SyncerImage {
		t.Errorf("syncer image = %q, want %q", syncerCont.Image, SyncerImage)
	}

	// Verify VCLUSTER_NAME env var
	foundEnv := false
	for _, env := range syncerCont.Env {
		if env.Name == "VCLUSTER_NAME" && env.Value == "test" {
			foundEnv = true
		}
	}
	if !foundEnv {
		t.Errorf("syncer container missing VCLUSTER_NAME env var")
	}

	// Verify syncer data volume is read-only
	for _, vm := range syncerCont.VolumeMounts {
		if vm.Name == "data" && !vm.ReadOnly {
			t.Errorf("syncer data volume mount should be read-only")
		}
	}

	// Verify service account
	if sts.Spec.Template.Spec.ServiceAccountName != "vc-test" {
		t.Errorf("service account = %q, want vc-test", sts.Spec.Template.Spec.ServiceAccountName)
	}

	// Verify no imagePullSecrets by default
	if len(sts.Spec.Template.Spec.ImagePullSecrets) != 0 {
		t.Errorf("expected no imagePullSecrets by default, got %d", len(sts.Spec.Template.Spec.ImagePullSecrets))
	}

	// Verify VolumeClaimTemplates
	if len(sts.Spec.VolumeClaimTemplates) != 1 {
		t.Fatalf("expected 1 VCT, got %d", len(sts.Spec.VolumeClaimTemplates))
	}
	if sts.Spec.VolumeClaimTemplates[0].Name != "data" {
		t.Errorf("VCT name = %q, want data", sts.Spec.VolumeClaimTemplates[0].Name)
	}
}

func TestCreateVirtualCluster_CustomSyncerImage(t *testing.T) {
	client := fake.NewSimpleClientset()
	ctx := context.Background()

	customImage := "registry.example.com/syncer:v1.2.3"
	err := CreateVirtualCluster(ctx, client, "custom", CreateOptions{
		SyncerImage: customImage,
	})
	if err != nil {
		t.Fatalf("CreateVirtualCluster failed: %v", err)
	}

	sts, err := client.AppsV1().StatefulSets("vc-custom").Get(ctx, "custom", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("statefulset not created: %v", err)
	}

	syncerCont := sts.Spec.Template.Spec.Containers[1]
	if syncerCont.Image != customImage {
		t.Errorf("syncer image = %q, want %q", syncerCont.Image, customImage)
	}
}

func TestCreateVirtualCluster_ImagePullSecret(t *testing.T) {
	// Pre-create the source secret
	client := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-registry",
			Namespace: "default",
		},
		Data: map[string][]byte{
			".dockerconfigjson": []byte(`{"auths":{}}`),
		},
		Type: corev1.SecretTypeDockerConfigJson,
	})
	ctx := context.Background()

	err := CreateVirtualCluster(ctx, client, "withsecret", CreateOptions{
		ImagePullSecret: "my-registry",
	})
	if err != nil {
		t.Fatalf("CreateVirtualCluster failed: %v", err)
	}

	// Verify secret was copied
	_, err = client.CoreV1().Secrets("vc-withsecret").Get(ctx, "my-registry", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("secret not copied to vcluster namespace: %v", err)
	}

	// Verify service account has imagePullSecrets
	sa, err := client.CoreV1().ServiceAccounts("vc-withsecret").Get(ctx, "vc-withsecret", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("service account not found: %v", err)
	}
	if len(sa.ImagePullSecrets) != 1 || sa.ImagePullSecrets[0].Name != "my-registry" {
		t.Errorf("service account imagePullSecrets = %+v, want [{my-registry}]", sa.ImagePullSecrets)
	}

	// Verify StatefulSet has imagePullSecrets
	sts, err := client.AppsV1().StatefulSets("vc-withsecret").Get(ctx, "withsecret", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("statefulset not found: %v", err)
	}
	if len(sts.Spec.Template.Spec.ImagePullSecrets) != 1 || sts.Spec.Template.Spec.ImagePullSecrets[0].Name != "my-registry" {
		t.Errorf("statefulset imagePullSecrets = %+v, want [{my-registry}]", sts.Spec.Template.Spec.ImagePullSecrets)
	}
}

func TestCreateVirtualCluster_DuplicateNamespace(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "vc-existing"},
	})
	ctx := context.Background()

	err := CreateVirtualCluster(ctx, client, "existing", CreateOptions{})
	if err == nil {
		t.Fatal("expected error for duplicate namespace, got nil")
	}
}

func TestDeleteVirtualCluster(t *testing.T) {
	// Set up a vcluster with all resources
	client := fake.NewSimpleClientset(
		&corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "vc-todelete",
				Labels: Labels("todelete"),
			},
		},
		&rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{Name: "vc-todelete-vc-todelete"},
		},
		&rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "vc-todelete-vc-todelete"},
		},
	)
	ctx := context.Background()

	err := DeleteVirtualCluster(ctx, client, "todelete")
	if err != nil {
		t.Fatalf("DeleteVirtualCluster failed: %v", err)
	}

	// Verify cluster-scoped resources deleted
	_, err = client.RbacV1().ClusterRoles().Get(ctx, "vc-todelete-vc-todelete", metav1.GetOptions{})
	if err == nil {
		t.Error("cluster role should have been deleted")
	}
	_, err = client.RbacV1().ClusterRoleBindings().Get(ctx, "vc-todelete-vc-todelete", metav1.GetOptions{})
	if err == nil {
		t.Error("cluster role binding should have been deleted")
	}

	// Verify namespace deleted
	_, err = client.CoreV1().Namespaces().Get(ctx, "vc-todelete", metav1.GetOptions{})
	if err == nil {
		t.Error("namespace should have been deleted")
	}
}

func TestDeleteVirtualCluster_NotFound(t *testing.T) {
	client := fake.NewSimpleClientset()
	ctx := context.Background()

	// Should not error on non-existent cluster
	err := DeleteVirtualCluster(ctx, client, "nonexistent")
	if err != nil {
		t.Fatalf("DeleteVirtualCluster should not error for non-existent cluster: %v", err)
	}
}

func TestListVirtualClusters_Empty(t *testing.T) {
	client := fake.NewSimpleClientset()
	ctx := context.Background()

	clusters, err := ListVirtualClusters(ctx, client)
	if err != nil {
		t.Fatalf("ListVirtualClusters failed: %v", err)
	}
	if len(clusters) != 0 {
		t.Errorf("expected 0 clusters, got %d", len(clusters))
	}
}

func TestListVirtualClusters(t *testing.T) {
	labels := Labels("mycluster")
	client := fake.NewSimpleClientset(
		&corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "vc-mycluster",
				Labels: labels,
				Annotations: map[string]string{
					AnnotationCreated: "2026-01-01T00:00:00Z",
				},
			},
		},
		&appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "mycluster",
				Namespace: "vc-mycluster",
			},
			Status: appsv1.StatefulSetStatus{
				ReadyReplicas: 1,
			},
		},
	)
	ctx := context.Background()

	clusters, err := ListVirtualClusters(ctx, client)
	if err != nil {
		t.Fatalf("ListVirtualClusters failed: %v", err)
	}
	if len(clusters) != 1 {
		t.Fatalf("expected 1 cluster, got %d", len(clusters))
	}

	c := clusters[0]
	if c.Name != "mycluster" {
		t.Errorf("name = %q, want mycluster", c.Name)
	}
	if c.Namespace != "vc-mycluster" {
		t.Errorf("namespace = %q, want vc-mycluster", c.Namespace)
	}
	if c.Status != "Running" {
		t.Errorf("status = %q, want Running", c.Status)
	}
	if c.Created != "2026-01-01T00:00:00Z" {
		t.Errorf("created = %q, want 2026-01-01T00:00:00Z", c.Created)
	}
}

func TestListVirtualClusters_PendingStatus(t *testing.T) {
	labels := Labels("pending")
	client := fake.NewSimpleClientset(
		&corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "vc-pending",
				Labels: labels,
			},
		},
		&appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pending",
				Namespace: "vc-pending",
			},
			Status: appsv1.StatefulSetStatus{
				ReadyReplicas: 0,
			},
		},
	)
	ctx := context.Background()

	clusters, err := ListVirtualClusters(ctx, client)
	if err != nil {
		t.Fatalf("ListVirtualClusters failed: %v", err)
	}
	if len(clusters) != 1 {
		t.Fatalf("expected 1 cluster, got %d", len(clusters))
	}
	if clusters[0].Status != "Pending" {
		t.Errorf("status = %q, want Pending", clusters[0].Status)
	}
}

func TestWaitForReady_AlreadyReady(t *testing.T) {
	client := fake.NewSimpleClientset(
		&appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "ready",
				Namespace: "vc-ready",
			},
			Status: appsv1.StatefulSetStatus{
				ReadyReplicas: 1,
			},
		},
	)
	ctx := context.Background()

	err := WaitForReady(ctx, client, "ready", 5e9)
	if err != nil {
		t.Fatalf("WaitForReady failed for already-ready cluster: %v", err)
	}
}

func TestK3sDisablesCoreDNS(t *testing.T) {
	// Issue #5: coredns must be in the --disable list because the virtual cluster
	// has no kubelet (--disable-agent) and no CNI (--flannel-backend=none), so the
	// coredns Deployment would never schedule, and the syncer skips kube-system.
	client := fake.NewSimpleClientset()
	ctx := context.Background()

	if err := CreateVirtualCluster(ctx, client, "dnstest", CreateOptions{}); err != nil {
		t.Fatalf("CreateVirtualCluster failed: %v", err)
	}

	sts, err := client.AppsV1().StatefulSets("vc-dnstest").Get(ctx, "dnstest", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("statefulset not created: %v", err)
	}

	k3sArgs := sts.Spec.Template.Spec.Containers[0].Args
	found := false
	for _, arg := range k3sArgs {
		if len(arg) > len("--disable=") && arg[:len("--disable=")] == "--disable=" {
			// arg looks like "--disable=traefik,servicelb,...,coredns"
			list := arg[len("--disable="):]
			for _, comp := range splitCSV(list) {
				if comp == "coredns" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Errorf("k3s args missing coredns in --disable list; args=%v", k3sArgs)
	}
}

// TestBuildStatefulSetDisablesCoreDNS covers the operator/builder path.
func TestBuildStatefulSetDisablesCoreDNS(t *testing.T) {
	sts := BuildStatefulSet(DefaultBuilderOptions("dnstest"))
	k3sArgs := sts.Spec.Template.Spec.Containers[0].Args
	found := false
	for _, arg := range k3sArgs {
		if len(arg) > len("--disable=") && arg[:len("--disable=")] == "--disable=" {
			list := arg[len("--disable="):]
			for _, comp := range splitCSV(list) {
				if comp == "coredns" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Errorf("BuildStatefulSet k3s args missing coredns in --disable list; args=%v", k3sArgs)
	}
}

func splitCSV(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}

func TestK3sTLSSANs(t *testing.T) {
	client := fake.NewSimpleClientset()
	ctx := context.Background()

	err := CreateVirtualCluster(ctx, client, "santest", CreateOptions{})
	if err != nil {
		t.Fatalf("CreateVirtualCluster failed: %v", err)
	}

	sts, _ := client.AppsV1().StatefulSets("vc-santest").Get(ctx, "santest", metav1.GetOptions{})
	k3sArgs := sts.Spec.Template.Spec.Containers[0].Args

	expectedSANs := []string{
		"--tls-san=santest.vc-santest.svc.cluster.local",
		"--tls-san=santest.vc-santest.svc",
		"--tls-san=santest",
	}
	for _, want := range expectedSANs {
		found := false
		for _, arg := range k3sArgs {
			if arg == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("k3s args missing TLS SAN %q", want)
		}
	}
}
