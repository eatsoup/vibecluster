package k8s

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
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
	SyncerImage        string
	ImagePullSecret    string // name of dockerconfigjson secret in default namespace to copy
	ExposeType         string
	ExposeIngressClass string
	ExposeHost         string
	// Resources caps the host resources the virtual cluster can consume.
	// When set, a ResourceQuota and a LimitRange are installed on the
	// vc-<name> namespace. The k3s control plane's own usage counts against
	// the budget; size accordingly. nil means no quota.
	Resources *ResourceLimits
	// VNode switches the cluster to the nested data-plane prototype: the
	// k3s server is configured with flannel + network-policy + servicelb,
	// the flat workload syncer is disabled, and a privileged Deployment
	// runs k3s agent joined to the virtual server so real workloads run
	// inside an in-vcluster kubelet. See issue #27.
	VNode bool
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
	if err := createServices(ctx, client, name, ns, labels, opts.ExposeType, opts.ExposeHost, opts.ExposeIngressClass); err != nil {
		return fmt.Errorf("creating services: %w", err)
	}

	// 6. Create ResourceQuota + LimitRange (if resources specified). This
	//    must happen *before* the StatefulSet so the LimitRange's defaults
	//    apply to the k3s/syncer pods at admission time — without that, the
	//    quota would reject them for having no requests/limits set.
	if !opts.Resources.IsEmpty() {
		fmt.Printf("  Creating ResourceQuota and LimitRange...\n")
		if err := createResourceLimits(ctx, client, name, ns, labels, opts.Resources); err != nil {
			return fmt.Errorf("creating resource limits: %w", err)
		}
	}

	// 7. Create statefulset
	fmt.Printf("  Creating StatefulSet...\n")
	if err := createStatefulSet(ctx, client, name, ns, labels, syncerImage, opts.ImagePullSecret, opts.ExposeHost, opts.VNode); err != nil {
		return fmt.Errorf("creating statefulset: %w", err)
	}

	// 8. In vnode mode, create the privileged k3s-agent Deployment that
	//    acts as the single virtual node for this prototype (issue #27).
	if opts.VNode {
		fmt.Printf("  Creating vnode agent Deployment...\n")
		if err := createVNodeDeployment(ctx, client, name, ns, labels, opts.ImagePullSecret); err != nil {
			return fmt.Errorf("creating vnode deployment: %w", err)
		}
	}

	return nil
}

// createResourceLimits installs the per-vcluster ResourceQuota and the
// matching LimitRange that supplies default container requests/limits so
// workloads without explicit resources are still admissible under the quota.
func createResourceLimits(ctx context.Context, client kubernetes.Interface, name, ns string, labels map[string]string, limits *ResourceLimits) error {
	opts := BuilderOptions{
		Name:      name,
		Namespace: ns,
		Labels:    labels,
		Resources: limits,
	}
	if rq := BuildResourceQuota(opts); rq != nil {
		if _, err := client.CoreV1().ResourceQuotas(ns).Create(ctx, rq, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("creating ResourceQuota: %w", err)
		}
	}
	if lr := BuildLimitRange(opts); lr != nil {
		if _, err := client.CoreV1().LimitRanges(ns).Create(ctx, lr, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("creating LimitRange: %w", err)
		}
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

func createServices(ctx context.Context, client kubernetes.Interface, name, ns string, labels map[string]string, exposeType, exposeHost, exposeIngressClass string) error {
	svcType := corev1.ServiceTypeClusterIP
	if exposeType == "LoadBalancer" {
		svcType = corev1.ServiceTypeLoadBalancer
	}

	// Main service
	_, err := client.CoreV1().Services(ns).Create(ctx, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: labels,
		},
		Spec: corev1.ServiceSpec{
			Type:     svcType,
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
	if err != nil {
		return err
	}

	if exposeType == "Ingress" {
		ing := BuildIngress(name, ns, labels, exposeHost, exposeIngressClass)
		if ing != nil {
			_, err = client.NetworkingV1().Ingresses(ns).Create(ctx, ing, metav1.CreateOptions{})
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func createStatefulSet(ctx context.Context, client kubernetes.Interface, name, ns string, labels map[string]string, syncerImage, imagePullSecret string, exposeHost string, vnode bool) error {
	k3sArgs := buildK3sServerArgs(name, ns, exposeHost, vnode)

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
					ServiceAccountName:            "vc-" + name,
					TerminationGracePeriodSeconds: ptr.To[int64](10),
					Containers: []corev1.Container{
						{
							Name:  "k3s",
							Image: K3sImage,
							Command: []string{
								"k3s",
							},
							Args: k3sArgs,
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
							Env:   buildSyncerEnv(name, vnode),
							Ports: []corev1.ContainerPort{
								{
									// Kubelet shim. The virtual k3s API
									// server dials this port (via the
									// patched InternalIP) when handling
									// logs/exec/portforward.
									Name:          "kubelet",
									ContainerPort: KubeletShimPort,
									Protocol:      corev1.ProtocolTCP,
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
									// Mounted read-only: the shim only
									// reads server-ca.crt / server-ca.key
									// to sign its serving cert; it never
									// writes anything under /data.
									ReadOnly: true,
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

// buildK3sServerArgs returns the arg list for the k3s server container. The
// vnode variant flips the networking defaults back on (flannel, network
// policy, servicelb, coredns) because a real agent Deployment is joining the
// cluster and will run those components — see createVNodeDeployment and
// issue #27.
func buildK3sServerArgs(name, ns, exposeHost string, vnode bool) []string {
	tlsSAN := []string{
		"--tls-san=" + name + "." + ns + ".svc.cluster.local",
		"--tls-san=" + name + "." + ns + ".svc",
		"--tls-san=" + name,
	}
	if exposeHost != "" {
		tlsSAN = append(tlsSAN, "--tls-san="+exposeHost)
	}

	if vnode {
		// Keep --disable-agent: the server pod itself is not a node. The
		// vnode Deployment is. Leave flannel + network-policy + servicelb
		// + coredns ON so NetworkPolicy and LoadBalancer Services work
		// inside the virtual cluster without any syncer translation.
		//
		// Pin non-default pod/service CIDRs so the nested cluster doesn't
		// collide with the host cluster (which on k3s also defaults to
		// 10.42/16 and 10.43/16). Collision breaks pod egress because the
		// host's flannel and the nested flannel both think they own the
		// same subnet. Static values for the prototype — productization
		// needs an allocator across multiple vclusters.
		args := []string{
			"server",
			"--disable=traefik,metrics-server,local-storage",
			"--disable-agent",
			"--disable-cloud-controller",
			"--disable-helm-controller",
			"--token=" + VNodeAgentToken(name),
			"--data-dir=/data/k3s",
			"--cluster-cidr=10.244.0.0/16",
			"--service-cidr=10.245.0.0/16",
			"--cluster-dns=10.245.0.10",
		}
		return append(args, tlsSAN...)
	}

	args := []string{
		"server",
		// coredns is disabled because the virtual cluster has no kubelet (--disable-agent)
		// and no CNI (--flannel-backend=none), so the coredns Deployment that k3s ships
		// would never schedule and stays Pending forever. The syncer also skips kube-system,
		// so it cannot translate the pod to the host. See issue #5.
		"--disable=traefik,servicelb,metrics-server,local-storage,coredns",
		"--disable-agent",
		"--disable-cloud-controller",
		"--disable-network-policy",
		"--disable-helm-controller",
		"--flannel-backend=none",
		// With --disable-agent there is no per-node tunnel for the apiserver
		// to dial kubelets through, so the default egress-selector mode
		// ("agent") makes kubectl logs/exec/portforward fail with a 502 the
		// instant kube-apiserver tries to reach a kubelet. Setting this to
		// "disabled" makes kube-apiserver dial kubelet addresses (i.e. our
		// shim) directly via TCP. See issue #21.
		"--egress-selector-mode=disabled",
		"--kube-controller-manager-arg=controllers=*,-nodeipam,-nodelifecycle,-persistentvolume-binder,-attachdetach,-persistentvolume-expander,-cloud-node-lifecycle,-ttl",
		// Force kube-apiserver to dial the kubelet by InternalIP. The
		// syncer rewrites every synced node's InternalIP to its own pod IP
		// (where the kubelet shim listens), so this is what makes
		// logs/exec/portforward route through the shim instead of the real
		// host kubelet — which doesn't know virtual pod names. See issue #21.
		"--kube-apiserver-arg=kubelet-preferred-address-types=InternalIP",
		"--data-dir=/data/k3s",
	}
	return append(args, tlsSAN...)
}

// buildSyncerEnv returns the env block for the syncer sidecar. The POD_IP
// entry is what the kubelet shim uses as its bind address / TLS SAN / the
// InternalIP it patches onto synced nodes; in vnode mode we also set
// EnvVNodeMode so the syncer skips every workload sync loop.
func buildSyncerEnv(name string, vnode bool) []corev1.EnvVar {
	env := []corev1.EnvVar{
		{Name: "VCLUSTER_NAME", Value: name},
		{
			Name: "POD_IP",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "status.podIP",
				},
			},
		},
	}
	if vnode {
		env = append(env, corev1.EnvVar{Name: EnvVNodeMode, Value: "true"})
	}
	return env
}

// VNodeAgentToken returns the deterministic k3s join token used for the
// prototype. One per-vcluster value, derived from the name, so the agent
// pod and server pod agree without any shared-secret bootstrap dance.
// Productization should replace this with a generated secret.
func VNodeAgentToken(name string) string {
	return "vibecluster-vnode-" + name
}

// createVNodeDeployment stands up the privileged k3s-agent Deployment that
// forms the single virtual node for a vnode-mode cluster (issue #27).
//
// The agent joins the virtual k3s server via the in-cluster Service DNS
// name (which is in the server cert's SAN list), using the deterministic
// per-vcluster token. One replica for the prototype; multi-node is
// productization scope.
//
// This pod needs privileged: true on a stock host cluster: the embedded
// kubelet mounts cgroups, the CNI (flannel) manipulates iptables/netlink,
// and containerd-in-containerd needs /dev access. Running non-privileged
// is feasible with Sysbox on the host — see the rootless investigation
// on issue #27 for why we are explicitly not doing that here.
func createVNodeDeployment(ctx context.Context, client kubernetes.Interface, name, ns string, labels map[string]string, imagePullSecret string) error {
	depName := name + "-vnode"
	// Intentionally do NOT copy the base labels — the main Service and the
	// headless Service select on `app: vibecluster`, so carrying that label
	// here would make the API server Service load-balance between the k3s
	// server pod and the vnode pod (which doesn't listen on 6443).
	depLabels := map[string]string{
		LabelManagedBy:              LabelManagedByValue,
		LabelVClusterName:           name,
		"vibecluster.dev/component": "vnode",
	}

	serverURL := "https://" + name + "." + ns + ".svc.cluster.local:" + fmt.Sprintf("%d", ServicePort)

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:   depName,
			Labels: depLabels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](1),
			Selector: &metav1.LabelSelector{MatchLabels: depLabels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: depLabels},
				Spec: corev1.PodSpec{
					ServiceAccountName:            "vc-" + name,
					TerminationGracePeriodSeconds: ptr.To[int64](10),
					// Nested containerd exhausts the host's default
					// fs.inotify.max_user_instances (128 on most distros)
					// almost immediately — the same issue kind/k3d document
					// for running Kubernetes-in-Docker. Bump it on the host
					// via a privileged init container so vibecluster carries
					// its own prerequisite instead of requiring a host-side
					// sysctl edit from the operator.
					InitContainers: []corev1.Container{
						{
							Name:  "sysctl",
							Image: VNodeAgentImage,
							Command: []string{"sh", "-c",
								"sysctl -w fs.inotify.max_user_instances=8192 && sysctl -w fs.inotify.max_user_watches=1048576",
							},
							SecurityContext: &corev1.SecurityContext{
								Privileged: ptr.To(true),
								RunAsUser:  ptr.To[int64](0),
							},
						},
					},
					Containers: []corev1.Container{
						{
							Name:    "k3s-agent",
							Image:   VNodeAgentImage,
							Command: []string{"k3s"},
							Args: []string{
								"agent",
								"--server=" + serverURL,
								"--token=" + VNodeAgentToken(name),
								"--node-name=$(NODE_NAME)",
							},
							Env: []corev1.EnvVar{
								{
									Name: "NODE_NAME",
									ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{
											FieldPath: "metadata.name",
										},
									},
								},
							},
							SecurityContext: &corev1.SecurityContext{
								Privileged: ptr.To(true),
								RunAsUser:  ptr.To[int64](0),
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("200m"),
									corev1.ResourceMemory: resource.MustParse("512Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceMemory: resource.MustParse("2Gi"),
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "k3s-data", MountPath: "/var/lib/rancher/k3s"},
								{Name: "kubelet", MountPath: "/var/lib/kubelet"},
								{Name: "cni-bin", MountPath: "/opt/cni/bin"},
								{Name: "cni-conf", MountPath: "/etc/cni/net.d"},
								{Name: "modules", MountPath: "/lib/modules", ReadOnly: true},
							},
						},
					},
					Volumes: []corev1.Volume{
						{Name: "k3s-data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{Name: "kubelet", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{Name: "cni-bin", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{Name: "cni-conf", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{
							Name: "modules",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{Path: "/lib/modules"},
							},
						},
					},
				},
			},
		},
	}

	if imagePullSecret != "" {
		dep.Spec.Template.Spec.ImagePullSecrets = []corev1.LocalObjectReference{
			{Name: imagePullSecret},
		}
	}

	_, err := client.AppsV1().Deployments(ns).Create(ctx, dep, metav1.CreateOptions{})
	return err
}

// BuildIngress returns an Ingress resource for the virtual cluster.
func BuildIngress(name, ns string, labels map[string]string, host, ingressClass string) *networkingv1.Ingress {
	pathType := networkingv1.PathTypeImplementationSpecific
	ingress := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    labels,
			Annotations: map[string]string{
				"nginx.ingress.kubernetes.io/ssl-passthrough":  "true",
				"nginx.ingress.kubernetes.io/backend-protocol": "HTTPS",
			},
		},
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{
				{
					Host: host,
					IngressRuleValue: networkingv1.IngressRuleValue{
						HTTP: &networkingv1.HTTPIngressRuleValue{
							Paths: []networkingv1.HTTPIngressPath{
								{
									Path:     "/",
									PathType: &pathType,
									Backend: networkingv1.IngressBackend{
										Service: &networkingv1.IngressServiceBackend{
											Name: name,
											Port: networkingv1.ServiceBackendPort{
												Number: ServicePort,
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	if ingressClass != "" {
		ingress.Spec.IngressClassName = ptr.To(ingressClass)
	}

	return ingress
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
//
// If the virtual cluster's namespace does not exist at all, this returns
// immediately with a NotFound error rather than spending the full timeout
// polling — see issue #19 (a typoed cluster name shouldn't take 30s to fail).
func WaitForReady(ctx context.Context, client kubernetes.Interface, name string, timeout time.Duration) error {
	ns := NamespaceName(name)

	// Fast-fail: if neither the namespace nor the StatefulSet exist, the
	// cluster simply doesn't exist. Return a clear NotFound immediately.
	if _, err := client.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{}); errors.IsNotFound(err) {
		return fmt.Errorf("virtual cluster %q not found (no namespace %s)", name, ns)
	} else if err != nil {
		return fmt.Errorf("checking namespace %s: %w", ns, err)
	}

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

// ExternalAddress describes how a virtual cluster's API can be reached from outside the host cluster.
type ExternalAddress struct {
	// Host is the hostname or IP to connect to.
	Host string
	// Port is the TCP port to connect to.
	Port int32
	// Source describes where the address came from ("LoadBalancer", "Ingress", or "" if none).
	Source string
	// CertVerifies is true when the address is expected to match a SAN in the k3s server cert.
	// When false, callers should set InsecureSkipTLSVerify on generated kubeconfigs.
	CertVerifies bool
}

// URL returns the https URL for connecting to the API server.
func (a ExternalAddress) URL() string {
	if a.Host == "" {
		return ""
	}
	if a.Port == 0 || a.Port == 443 {
		return fmt.Sprintf("https://%s", a.Host)
	}
	return fmt.Sprintf("https://%s:%d", a.Host, a.Port)
}

// GetExternalAddress returns the externally-reachable address for a virtual cluster, if any.
// For LoadBalancer services it reads svc.Status.LoadBalancer.Ingress.
// For Ingress exposure it reads the Ingress rule host.
// Returns an empty ExternalAddress (zero Host) if the cluster is not exposed externally.
func GetExternalAddress(ctx context.Context, client kubernetes.Interface, name string) (ExternalAddress, error) {
	ns := NamespaceName(name)

	svc, err := client.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return ExternalAddress{}, fmt.Errorf("getting service: %w", err)
	}

	if svc.Spec.Type == corev1.ServiceTypeLoadBalancer {
		for _, ing := range svc.Status.LoadBalancer.Ingress {
			host := ing.Hostname
			if host == "" {
				host = ing.IP
			}
			if host != "" {
				// LB IPs/hostnames are not in the k3s cert SAN by default — connections
				// must skip TLS verification unless the user added the address to TLS-SANs.
				return ExternalAddress{Host: host, Port: ServicePort, Source: "LoadBalancer", CertVerifies: false}, nil
			}
		}
		return ExternalAddress{Source: "LoadBalancer"}, nil
	}

	ing, err := client.NetworkingV1().Ingresses(ns).Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		for _, rule := range ing.Spec.Rules {
			if rule.Host != "" {
				// Ingress hosts are added to TLS-SAN at create/expose time, so the cert validates.
				return ExternalAddress{Host: rule.Host, Port: 443, Source: "Ingress", CertVerifies: true}, nil
			}
		}
	} else if !errors.IsNotFound(err) {
		return ExternalAddress{}, fmt.Errorf("getting ingress: %w", err)
	}

	return ExternalAddress{}, nil
}

// WaitForExternalAddress polls until GetExternalAddress returns a non-empty Host or the timeout elapses.
func WaitForExternalAddress(ctx context.Context, client kubernetes.Interface, name string, timeout time.Duration) (ExternalAddress, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		addr, err := GetExternalAddress(ctx, client, name)
		if err != nil {
			return ExternalAddress{}, err
		}
		if addr.Host != "" {
			return addr, nil
		}
		select {
		case <-ctx.Done():
			return ExternalAddress{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return ExternalAddress{}, fmt.Errorf("timeout waiting for external address for %s", name)
}
