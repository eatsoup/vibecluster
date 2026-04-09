package k8s

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
)

// CreateOptions holds options for virtual cluster creation.
type CreateOptions struct {
	SyncerImage     string
	ImagePullSecret string // name of dockerconfigjson secret in default namespace to copy
}

// CreateVirtualCluster deploys all resources for a virtual cluster.
func CreateVirtualCluster(ctx context.Context, client kubernetes.Interface, name string, opts CreateOptions) error {
	ns := NamespaceName(name)
	labels := Labels(name)
	annotations := map[string]string{
		AnnotationCreated: time.Now().UTC().Format(time.RFC3339),
	}

	syncerImage := opts.SyncerImage
	if syncerImage == "" {
		syncerImage = SyncerImage
	}

	// 1. Create namespace
	fmt.Printf("  Creating namespace %s...\n", ns)
	if err := createNamespace(ctx, client, ns, labels, annotations); err != nil {
		return fmt.Errorf("creating namespace: %w", err)
	}

	// 2. Copy image pull secret into the namespace if specified
	if opts.ImagePullSecret != "" {
		fmt.Printf("  Copying image pull secret %q...\n", opts.ImagePullSecret)
		if err := copySecret(ctx, client, opts.ImagePullSecret, "default", ns); err != nil {
			return fmt.Errorf("copying image pull secret: %w", err)
		}
	}

	// 3. Create service account
	fmt.Printf("  Creating service account...\n")
	if err := createServiceAccount(ctx, client, name, ns, labels, opts.ImagePullSecret); err != nil {
		return fmt.Errorf("creating service account: %w", err)
	}

	// 4. Create RBAC
	fmt.Printf("  Creating RBAC resources...\n")
	if err := createRBAC(ctx, client, name, ns, labels); err != nil {
		return fmt.Errorf("creating RBAC: %w", err)
	}

	// 5. Create services
	fmt.Printf("  Creating services...\n")
	if err := createServices(ctx, client, name, ns, labels); err != nil {
		return fmt.Errorf("creating services: %w", err)
	}

	// 6. Create statefulset
	fmt.Printf("  Creating StatefulSet...\n")
	if err := createStatefulSet(ctx, client, name, ns, labels, syncerImage, opts.ImagePullSecret); err != nil {
		return fmt.Errorf("creating statefulset: %w", err)
	}

	return nil
}

// DeleteVirtualCluster removes all resources for a virtual cluster.
func DeleteVirtualCluster(ctx context.Context, client kubernetes.Interface, name string) error {
	ns := NamespaceName(name)

	// Delete cluster-scoped RBAC first
	clusterRoleName := fmt.Sprintf("vc-%s-%s", name, ns)
	_ = client.RbacV1().ClusterRoleBindings().Delete(ctx, clusterRoleName, metav1.DeleteOptions{})
	_ = client.RbacV1().ClusterRoles().Delete(ctx, clusterRoleName, metav1.DeleteOptions{})

	// Delete namespace (cascades to all namespaced resources)
	err := client.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("deleting namespace %s: %w", ns, err)
	}

	return nil
}

func createNamespace(ctx context.Context, client kubernetes.Interface, ns string, labels, annotations map[string]string) error {
	_, err := client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:        ns,
			Labels:      labels,
			Annotations: annotations,
		},
	}, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		return fmt.Errorf("virtual cluster namespace %s already exists", ns)
	}
	return err
}

func copySecret(ctx context.Context, client kubernetes.Interface, name, srcNS, dstNS string) error {
	secret, err := client.CoreV1().Secrets(srcNS).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting secret %s/%s: %w", srcNS, name, err)
	}

	newSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secret.Name,
			Namespace: dstNS,
		},
		Data: secret.Data,
		Type: secret.Type,
	}

	_, err = client.CoreV1().Secrets(dstNS).Create(ctx, newSecret, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func createServiceAccount(ctx context.Context, client kubernetes.Interface, name, ns string, labels map[string]string, imagePullSecret string) error {
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "vc-" + name,
			Labels: labels,
		},
	}
	if imagePullSecret != "" {
		sa.ImagePullSecrets = []corev1.LocalObjectReference{
			{Name: imagePullSecret},
		}
	}
	_, err := client.CoreV1().ServiceAccounts(ns).Create(ctx, sa, metav1.CreateOptions{})
	return err
}

func createRBAC(ctx context.Context, client kubernetes.Interface, name, ns string, labels map[string]string) error {
	clusterRoleName := fmt.Sprintf("vc-%s-%s", name, ns)

	// ClusterRole - permissions needed by the syncer to operate on host cluster
	_, err := client.RbacV1().ClusterRoles().Create(ctx, &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name:   clusterRoleName,
			Labels: labels,
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{"pods", "pods/status", "pods/log", "pods/exec", "pods/attach", "pods/portforward"},
				Verbs:     []string{"*"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"services", "endpoints", "configmaps", "secrets", "serviceaccounts"},
				Verbs:     []string{"*"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"events"},
				Verbs:     []string{"create", "patch", "update"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"persistentvolumeclaims"},
				Verbs:     []string{"*"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"nodes", "nodes/status", "nodes/metrics", "nodes/proxy"},
				Verbs:     []string{"get", "list", "watch"},
			},
			{
				APIGroups: []string{"apps"},
				Resources: []string{"statefulsets", "deployments", "replicasets", "daemonsets"},
				Verbs:     []string{"get", "list", "watch"},
			},
			{
				APIGroups: []string{"networking.k8s.io"},
				Resources: []string{"ingresses"},
				Verbs:     []string{"*"},
			},
			{
				APIGroups: []string{"storage.k8s.io"},
				Resources: []string{"storageclasses", "csinodes", "csidrivers"},
				Verbs:     []string{"get", "list", "watch"},
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return err
	}

	// ClusterRoleBinding
	_, err = client.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:   clusterRoleName,
			Labels: labels,
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     clusterRoleName,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      "vc-" + name,
				Namespace: ns,
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return err
	}

	// Namespace-scoped role for full access within the vcluster namespace
	_, err = client.RbacV1().Roles(ns).Create(ctx, &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "vc-" + name,
			Labels: labels,
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{"*"},
				Resources: []string{"*"},
				Verbs:     []string{"*"},
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return err
	}

	_, err = client.RbacV1().RoleBindings(ns).Create(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "vc-" + name,
			Labels: labels,
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     "vc-" + name,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      "vc-" + name,
				Namespace: ns,
			},
		},
	}, metav1.CreateOptions{})
	return err
}

func createServices(ctx context.Context, client kubernetes.Interface, name, ns string, labels map[string]string) error {
	// Main service (ClusterIP)
	_, err := client.CoreV1().Services(ns).Create(ctx, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: labels,
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: labels,
			Ports: []corev1.ServicePort{
				{
					Name:       "https",
					Port:       ServicePort,
					TargetPort: intstr.FromInt32(K3sPort),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return err
	}

	// Headless service for StatefulSet
	_, err = client.CoreV1().Services(ns).Create(ctx, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name + "-headless",
			Labels: labels,
		},
		Spec: corev1.ServiceSpec{
			Type:      corev1.ServiceTypeClusterIP,
			ClusterIP: "None",
			Selector:  labels,
			Ports: []corev1.ServicePort{
				{
					Name:       "https",
					Port:       ServicePort,
					TargetPort: intstr.FromInt32(K3sPort),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}, metav1.CreateOptions{})
	return err
}

func createStatefulSet(ctx context.Context, client kubernetes.Interface, name, ns string, labels map[string]string, syncerImage, imagePullSecret string) error {
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: labels,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    ptr.To[int32](1),
			ServiceName: name + "-headless",
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					ServiceAccountName:           "vc-" + name,
					TerminationGracePeriodSeconds: ptr.To[int64](10),
					Containers: []corev1.Container{
						{
							Name:  "k3s",
							Image: K3sImage,
							Command: []string{
								"k3s",
							},
							Args: []string{
								"server",
								"--disable=traefik,servicelb,metrics-server,local-storage",
								"--disable-agent",
								"--disable-cloud-controller",
								"--disable-network-policy",
								"--disable-helm-controller",
								"--flannel-backend=none",
								"--kube-controller-manager-arg=controllers=*,-nodeipam,-nodelifecycle,-persistentvolume-binder,-attachdetach,-persistentvolume-expander,-cloud-node-lifecycle,-ttl",
								"--data-dir=/data/k3s",
								"--tls-san=" + name + "." + ns + ".svc.cluster.local",
								"--tls-san=" + name + "." + ns + ".svc",
								"--tls-san=" + name,
							},
							Ports: []corev1.ContainerPort{
								{
									Name:          "https",
									ContainerPort: K3sPort,
									Protocol:      corev1.ProtocolTCP,
								},
							},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									Exec: &corev1.ExecAction{
										Command: []string{"kubectl", "--kubeconfig=/data/k3s/server/cred/admin.kubeconfig", "get", "--raw", "/readyz"},
									},
								},
								InitialDelaySeconds: 15,
								PeriodSeconds:       5,
								FailureThreshold:    24,
							},
							LivenessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									Exec: &corev1.ExecAction{
										Command: []string{"kubectl", "--kubeconfig=/data/k3s/server/cred/admin.kubeconfig", "get", "--raw", "/livez"},
									},
								},
								InitialDelaySeconds: 60,
								PeriodSeconds:       10,
								FailureThreshold:    6,
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: resource.MustParse("256Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceMemory: resource.MustParse("1Gi"),
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "data",
									MountPath: "/data",
								},
							},
							SecurityContext: &corev1.SecurityContext{
								Privileged: ptr.To(false),
								RunAsUser:  ptr.To[int64](0),
							},
						},
						{
							Name:  "syncer",
							Image: syncerImage,
							Env: []corev1.EnvVar{
								{
									Name:  "VCLUSTER_NAME",
									Value: name,
								},
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("50m"),
									corev1.ResourceMemory: resource.MustParse("64Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceMemory: resource.MustParse("256Mi"),
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "data",
									MountPath: "/data",
									ReadOnly:  true,
								},
							},
						},
					},
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "data",
					},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{
							corev1.ReadWriteOnce,
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: resource.MustParse("5Gi"),
							},
						},
					},
				},
			},
		},
	}

	if imagePullSecret != "" {
		sts.Spec.Template.Spec.ImagePullSecrets = []corev1.LocalObjectReference{
			{Name: imagePullSecret},
		}
	}

	_, err := client.AppsV1().StatefulSets(ns).Create(ctx, sts, metav1.CreateOptions{})
	return err
}

// ListVirtualClusters returns info about all virtual clusters.
type VClusterInfo struct {
	Name      string
	Namespace string
	Status    string
	Created   string
}

func ListVirtualClusters(ctx context.Context, client kubernetes.Interface) ([]VClusterInfo, error) {
	namespaces, err := client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{
		LabelSelector: LabelManagedBy + "=" + LabelManagedByValue,
	})
	if err != nil {
		return nil, fmt.Errorf("listing namespaces: %w", err)
	}

	var clusters []VClusterInfo
	for _, ns := range namespaces.Items {
		name := ns.Labels[LabelVClusterName]
		if name == "" {
			continue
		}

		status := "Unknown"
		sts, err := client.AppsV1().StatefulSets(ns.Name).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			if sts.Status.ReadyReplicas > 0 {
				status = "Running"
			} else {
				status = "Pending"
			}
		}

		created := ns.Annotations[AnnotationCreated]

		clusters = append(clusters, VClusterInfo{
			Name:      name,
			Namespace: ns.Name,
			Status:    status,
			Created:   created,
		})
	}

	return clusters, nil
}

// WaitForReady waits until the virtual cluster pod is ready.
func WaitForReady(ctx context.Context, client kubernetes.Interface, name string, timeout time.Duration) error {
	ns := NamespaceName(name)
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		sts, err := client.AppsV1().StatefulSets(ns).Get(ctx, name, metav1.GetOptions{})
		if err == nil && sts.Status.ReadyReplicas > 0 {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}

	return fmt.Errorf("timeout waiting for virtual cluster %s to become ready", name)
}
