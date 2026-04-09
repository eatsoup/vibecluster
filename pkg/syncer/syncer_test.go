package syncer

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestHostName(t *testing.T) {
	s := New("mycluster", nil, nil)

	tests := []struct {
		name      string
		namespace string
		expected  string
	}{
		{"my-pod", "default", "mycluster-x-my-pod-x-default"},
		{"nginx", "production", "mycluster-x-nginx-x-production"},
		{"a", "b", "mycluster-x-a-x-b"},
	}

	for _, tt := range tests {
		got := s.HostName(tt.name, tt.namespace)
		if got != tt.expected {
			t.Errorf("HostName(%q, %q) = %q, want %q", tt.name, tt.namespace, got, tt.expected)
		}
	}
}

func TestSyncLabels(t *testing.T) {
	s := New("test", nil, nil)
	labels := s.syncLabels("my-pod", "default")

	if labels[LabelSyncedFrom] != "test" {
		t.Errorf("synced-from = %q, want test", labels[LabelSyncedFrom])
	}
	if labels[LabelVirtualName] != "my-pod" {
		t.Errorf("virtual-name = %q, want my-pod", labels[LabelVirtualName])
	}
	if labels[LabelVirtualNamespace] != "default" {
		t.Errorf("virtual-namespace = %q, want default", labels[LabelVirtualNamespace])
	}
	if labels["app"] != "vibecluster" {
		t.Errorf("app = %q, want vibecluster", labels["app"])
	}
}

func TestIsSystemNamespace(t *testing.T) {
	systemNS := []string{"kube-system", "kube-public", "kube-node-lease"}
	for _, ns := range systemNS {
		if !isSystemNamespace(ns) {
			t.Errorf("isSystemNamespace(%q) = false, want true", ns)
		}
	}

	nonSystemNS := []string{"default", "my-app", "production"}
	for _, ns := range nonSystemNS {
		if isSystemNamespace(ns) {
			t.Errorf("isSystemNamespace(%q) = true, want false", ns)
		}
	}
}

func TestIsSystemSecret(t *testing.T) {
	tests := []struct {
		name     string
		secret   *corev1.Secret
		expected bool
	}{
		{
			"service account token",
			&corev1.Secret{Type: corev1.SecretTypeServiceAccountToken},
			true,
		},
		{
			"k3s internal",
			&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "k3s-serving"},
				Type:       corev1.SecretTypeOpaque,
			},
			true,
		},
		{
			"user secret",
			&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "my-api-key"},
				Type:       corev1.SecretTypeOpaque,
			},
			false,
		},
		{
			"docker config",
			&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "registry-creds"},
				Type:       corev1.SecretTypeDockerConfigJson,
			},
			false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isSystemSecret(tt.secret)
			if got != tt.expected {
				t.Errorf("isSystemSecret(%q) = %v, want %v", tt.name, got, tt.expected)
			}
		})
	}
}

func TestSyncConfigMapToHost(t *testing.T) {
	hostClient := fake.NewSimpleClientset()
	vClient := fake.NewSimpleClientset()
	s := New("test", hostClient, vClient)
	ctx := context.Background()

	vCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "app-config",
			Namespace: "default",
		},
		Data: map[string]string{
			"key1": "value1",
			"key2": "value2",
		},
	}

	err := s.syncConfigMapToHost(ctx, vCM)
	if err != nil {
		t.Fatalf("syncConfigMapToHost failed: %v", err)
	}

	// Verify on host
	hostName := "test-x-app-config-x-default"
	hostCM, err := hostClient.CoreV1().ConfigMaps("vc-test").Get(ctx, hostName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("host configmap not found: %v", err)
	}

	if hostCM.Data["key1"] != "value1" {
		t.Errorf("key1 = %q, want value1", hostCM.Data["key1"])
	}
	if hostCM.Data["key2"] != "value2" {
		t.Errorf("key2 = %q, want value2", hostCM.Data["key2"])
	}
	if hostCM.Labels[LabelSyncedFrom] != "test" {
		t.Errorf("missing synced-from label")
	}
	if hostCM.Labels[LabelVirtualName] != "app-config" {
		t.Errorf("missing virtual-name label")
	}
}

func TestSyncConfigMapToHost_Update(t *testing.T) {
	hostClient := fake.NewSimpleClientset()
	vClient := fake.NewSimpleClientset()
	s := New("test", hostClient, vClient)
	ctx := context.Background()

	vCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "updatable",
			Namespace: "default",
		},
		Data: map[string]string{"version": "1"},
	}

	// First sync
	if err := s.syncConfigMapToHost(ctx, vCM); err != nil {
		t.Fatalf("first sync failed: %v", err)
	}

	// Update and sync again
	vCM.Data["version"] = "2"
	if err := s.syncConfigMapToHost(ctx, vCM); err != nil {
		t.Fatalf("second sync failed: %v", err)
	}

	hostCM, _ := hostClient.CoreV1().ConfigMaps("vc-test").Get(ctx, "test-x-updatable-x-default", metav1.GetOptions{})
	if hostCM.Data["version"] != "2" {
		t.Errorf("version = %q, want 2", hostCM.Data["version"])
	}
}

func TestSyncSecretToHost(t *testing.T) {
	hostClient := fake.NewSimpleClientset()
	vClient := fake.NewSimpleClientset()
	s := New("test", hostClient, vClient)
	ctx := context.Background()

	vSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "db-creds",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"username": []byte("admin"),
			"password": []byte("secret"),
		},
		Type: corev1.SecretTypeOpaque,
	}

	err := s.syncSecretToHost(ctx, vSecret)
	if err != nil {
		t.Fatalf("syncSecretToHost failed: %v", err)
	}

	hostSecret, err := hostClient.CoreV1().Secrets("vc-test").Get(ctx, "test-x-db-creds-x-default", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("host secret not found: %v", err)
	}

	if string(hostSecret.Data["username"]) != "admin" {
		t.Errorf("username = %q, want admin", hostSecret.Data["username"])
	}
	if hostSecret.Type != corev1.SecretTypeOpaque {
		t.Errorf("type = %q, want Opaque", hostSecret.Type)
	}
	if hostSecret.Labels[LabelSyncedFrom] != "test" {
		t.Errorf("missing synced-from label")
	}
}

func TestSyncServiceToHost(t *testing.T) {
	hostClient := fake.NewSimpleClientset()
	vClient := fake.NewSimpleClientset()
	s := New("test", hostClient, vClient)
	ctx := context.Background()

	vSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web",
			Namespace: "default",
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
			Selector: map[string]string{
				"app": "web",
			},
			Ports: []corev1.ServicePort{
				{Port: 80, Protocol: corev1.ProtocolTCP},
			},
		},
	}

	err := s.syncServiceToHost(ctx, vSvc)
	if err != nil {
		t.Fatalf("syncServiceToHost failed: %v", err)
	}

	hostSvc, err := hostClient.CoreV1().Services("vc-test").Get(ctx, "test-x-web-x-default", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("host service not found: %v", err)
	}

	// Should have original selector plus synced-from
	if hostSvc.Spec.Selector["app"] != "web" {
		t.Errorf("missing app selector")
	}
	if hostSvc.Spec.Selector[LabelSyncedFrom] != "test" {
		t.Errorf("missing synced-from selector")
	}
	if len(hostSvc.Spec.Ports) != 1 || hostSvc.Spec.Ports[0].Port != 80 {
		t.Errorf("ports unexpected: %+v", hostSvc.Spec.Ports)
	}
}

func TestSyncServiceToHost_Update(t *testing.T) {
	hostClient := fake.NewSimpleClientset()
	vClient := fake.NewSimpleClientset()
	s := New("test", hostClient, vClient)
	ctx := context.Background()

	vSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: map[string]string{"app": "api"},
			Ports:    []corev1.ServicePort{{Port: 8080, Protocol: corev1.ProtocolTCP}},
		},
	}

	if err := s.syncServiceToHost(ctx, vSvc); err != nil {
		t.Fatalf("first sync failed: %v", err)
	}

	// Update port
	vSvc.Spec.Ports[0].Port = 9090
	if err := s.syncServiceToHost(ctx, vSvc); err != nil {
		t.Fatalf("second sync failed: %v", err)
	}

	hostSvc, _ := hostClient.CoreV1().Services("vc-test").Get(ctx, "test-x-api-x-default", metav1.GetOptions{})
	if hostSvc.Spec.Ports[0].Port != 9090 {
		t.Errorf("port = %d, want 9090", hostSvc.Spec.Ports[0].Port)
	}
}

func TestSyncPodToHost(t *testing.T) {
	hostClient := fake.NewSimpleClientset()
	vClient := fake.NewSimpleClientset()
	s := New("test", hostClient, vClient)
	ctx := context.Background()

	vPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "nginx",
			Namespace: "default",
			Labels:    map[string]string{"app": "web"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "nginx",
					Image: "nginx:alpine",
				},
			},
		},
	}

	err := s.syncPodToHost(ctx, vPod)
	if err != nil {
		t.Fatalf("syncPodToHost failed: %v", err)
	}

	hostPod, err := hostClient.CoreV1().Pods("vc-test").Get(ctx, "test-x-nginx-x-default", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("host pod not found: %v", err)
	}

	// Check labels preserved
	if hostPod.Labels["app"] != "web" {
		t.Errorf("app label not preserved")
	}
	if hostPod.Labels[LabelSyncedFrom] != "test" {
		t.Errorf("missing synced-from label")
	}

	// Check service account overridden
	if hostPod.Spec.ServiceAccountName != "vc-test" {
		t.Errorf("service account = %q, want vc-test", hostPod.Spec.ServiceAccountName)
	}

	// Check automount disabled
	if hostPod.Spec.AutomountServiceAccountToken == nil || *hostPod.Spec.AutomountServiceAccountToken {
		t.Error("automount should be disabled")
	}

	// Check container image preserved
	if hostPod.Spec.Containers[0].Image != "nginx:alpine" {
		t.Errorf("image = %q, want nginx:alpine", hostPod.Spec.Containers[0].Image)
	}
}

func TestSyncPodToHost_FiltersSATokenVolumes(t *testing.T) {
	hostClient := fake.NewSimpleClientset()
	vClient := fake.NewSimpleClientset()
	s := New("test", hostClient, vClient)
	ctx := context.Background()

	vPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "withtoken",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "app",
					Image: "myapp:latest",
					VolumeMounts: []corev1.VolumeMount{
						{Name: "data", MountPath: "/data"},
						{Name: "kube-api-access-abc", MountPath: "/var/run/secrets"},
					},
				},
			},
			Volumes: []corev1.Volume{
				{Name: "data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				{Name: "kube-api-access-abc", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{}}},
			},
		},
	}

	if err := s.syncPodToHost(ctx, vPod); err != nil {
		t.Fatalf("syncPodToHost failed: %v", err)
	}

	hostPod, _ := hostClient.CoreV1().Pods("vc-test").Get(ctx, "test-x-withtoken-x-default", metav1.GetOptions{})

	// Should only have "data" volume, not kube-api-access
	if len(hostPod.Spec.Volumes) != 1 {
		t.Errorf("expected 1 volume, got %d: %+v", len(hostPod.Spec.Volumes), hostPod.Spec.Volumes)
	}
	if len(hostPod.Spec.Containers[0].VolumeMounts) != 1 {
		t.Errorf("expected 1 volume mount, got %d", len(hostPod.Spec.Containers[0].VolumeMounts))
	}
}

func TestSyncPodToHost_RewritesVolumeRefs(t *testing.T) {
	hostClient := fake.NewSimpleClientset()
	vClient := fake.NewSimpleClientset()
	s := New("test", hostClient, vClient)
	ctx := context.Background()

	vPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "withvols",
			Namespace: "myns",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Image: "app:latest"},
			},
			Volumes: []corev1.Volume{
				{
					Name: "config",
					VolumeSource: corev1.VolumeSource{
						ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: "app-config"},
						},
					},
				},
				{
					Name: "creds",
					VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{
							SecretName: "app-secret",
						},
					},
				},
			},
		},
	}

	if err := s.syncPodToHost(ctx, vPod); err != nil {
		t.Fatalf("syncPodToHost failed: %v", err)
	}

	hostPod, _ := hostClient.CoreV1().Pods("vc-test").Get(ctx, "test-x-withvols-x-myns", metav1.GetOptions{})

	// ConfigMap volume should be rewritten
	cmVol := hostPod.Spec.Volumes[0]
	if cmVol.ConfigMap.Name != "test-x-app-config-x-myns" {
		t.Errorf("configmap volume name = %q, want test-x-app-config-x-myns", cmVol.ConfigMap.Name)
	}

	// Secret volume should be rewritten
	secVol := hostPod.Spec.Volumes[1]
	if secVol.Secret.SecretName != "test-x-app-secret-x-myns" {
		t.Errorf("secret volume name = %q, want test-x-app-secret-x-myns", secVol.Secret.SecretName)
	}
}

func TestSyncNodeToVirtual(t *testing.T) {
	hostClient := fake.NewSimpleClientset()
	vClient := fake.NewSimpleClientset()
	s := New("test", hostClient, vClient)
	ctx := context.Background()

	hostNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "worker-1",
			Labels: map[string]string{
				"kubernetes.io/os":   "linux",
				"kubernetes.io/arch": "amd64",
			},
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	}

	err := s.syncNodeToVirtual(ctx, hostNode)
	if err != nil {
		t.Fatalf("syncNodeToVirtual failed: %v", err)
	}

	vNode, err := vClient.CoreV1().Nodes().Get(ctx, "worker-1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("virtual node not found: %v", err)
	}

	if vNode.Labels["kubernetes.io/os"] != "linux" {
		t.Errorf("os label not synced")
	}
	if len(vNode.Status.Conditions) != 1 || vNode.Status.Conditions[0].Type != corev1.NodeReady {
		t.Errorf("node conditions not synced")
	}
}

func TestSyncNodeToVirtual_Update(t *testing.T) {
	vClient := fake.NewSimpleClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-1"},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionFalse},
			},
		},
	})
	s := New("test", nil, vClient)
	ctx := context.Background()

	hostNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "worker-1",
			Labels: map[string]string{"updated": "true"},
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	}

	if err := s.syncNodeToVirtual(ctx, hostNode); err != nil {
		t.Fatalf("syncNodeToVirtual update failed: %v", err)
	}

	vNode, _ := vClient.CoreV1().Nodes().Get(ctx, "worker-1", metav1.GetOptions{})
	if vNode.Status.Conditions[0].Status != corev1.ConditionTrue {
		t.Errorf("node status not updated")
	}
}

func TestNew(t *testing.T) {
	hostClient := fake.NewSimpleClientset()
	vClient := fake.NewSimpleClientset()

	s := New("myvc", hostClient, vClient)

	if s.name != "myvc" {
		t.Errorf("name = %q, want myvc", s.name)
	}
	if s.hostNS != "vc-myvc" {
		t.Errorf("hostNS = %q, want vc-myvc", s.hostNS)
	}
}

func TestWatchWithRetry_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	err := watchWithRetry(ctx, "test", func(ctx context.Context) error {
		<-ctx.Done()
		return nil
	})

	if err != nil {
		t.Errorf("expected nil error on context cancel, got %v", err)
	}
}
