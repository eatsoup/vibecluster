package k8s

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

// BuilderOptions holds configurable parameters for building virtual cluster resources.
// These are used by both the CLI and the operator.
type BuilderOptions struct {
	// Name is the virtual cluster name.
	Name string
	// Namespace is the host namespace (vc-<name>).
	Namespace string
	// Labels are the standard labels for the resources.
	Labels map[string]string
	// K3sImage overrides the default k3s image.
	K3sImage string
	// SyncerImage overrides the default syncer image.
	SyncerImage string
	// Storage overrides the default PVC storage size.
	Storage string
	// ImagePullSecret is the name of a dockerconfigjson secret for image pulls.
	ImagePullSecret string
	// ExposeType configures the API service exposure: "" (ClusterIP only),
	// "LoadBalancer", or "Ingress".
	ExposeType string
	// ExposeHost is the external hostname for Ingress exposure. When set, it
	// is appended to the k3s server certificate's TLS-SAN list so a kubeconfig
	// pointing at the host validates against the cluster's serving cert.
	ExposeHost string
	// ExposeIngressClass is the IngressClassName to use when ExposeType is "Ingress".
	ExposeIngressClass string
	// Resources caps the total CPU/memory/storage/pods the virtual cluster
	// is allowed to consume on the host. When nil (or all fields empty), no
	// quota is installed and the cluster can use whatever the host scheduler
	// gives it. The k3s control plane's own usage counts against the budget.
	Resources *ResourceLimits
}

// ResourceLimits is the per-vcluster resource budget enforced via a
// namespace-scoped ResourceQuota.
type ResourceLimits struct {
	// CPU is the total CPU budget (e.g. "4", "500m"). Empty means unlimited.
	CPU string
	// Memory is the total memory budget (e.g. "8Gi"). Empty means unlimited.
	Memory string
	// Storage is the total persistent storage budget across all PVCs
	// (e.g. "50Gi"). Empty means unlimited.
	Storage string
	// Pods is the maximum pod count. Zero means unlimited.
	Pods int32
}

// IsEmpty reports whether the resource budget has no fields set, in which
// case no ResourceQuota should be installed.
func (r *ResourceLimits) IsEmpty() bool {
	if r == nil {
		return true
	}
	return r.CPU == "" && r.Memory == "" && r.Storage == "" && r.Pods == 0
}

// DefaultBuilderOptions returns BuilderOptions with sensible defaults for the given name.
func DefaultBuilderOptions(name string) BuilderOptions {
	return BuilderOptions{
		Name:        name,
		Namespace:   NamespaceName(name),
		Labels:      Labels(name),
		K3sImage:    K3sImage,
		SyncerImage: SyncerImage,
		Storage:     "5Gi",
	}
}

// BuildNamespace returns a Namespace object for the virtual cluster.
func BuildNamespace(opts BuilderOptions, annotations map[string]string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:        opts.Namespace,
			Labels:      opts.Labels,
			Annotations: annotations,
		},
	}
}

// BuildServiceAccount returns a ServiceAccount for the virtual cluster syncer.
func BuildServiceAccount(opts BuilderOptions) *corev1.ServiceAccount {
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "vc-" + opts.Name,
			Namespace: opts.Namespace,
			Labels:    opts.Labels,
		},
	}
	if opts.ImagePullSecret != "" {
		sa.ImagePullSecrets = []corev1.LocalObjectReference{
			{Name: opts.ImagePullSecret},
		}
	}
	return sa
}

// ClusterRoleName returns the cluster role name for a virtual cluster.
func ClusterRoleName(name, ns string) string {
	return "vc-" + name + "-" + ns
}

// SyncerClusterRoleRules returns the policy rules granted to the per-vcluster
// syncer ClusterRole. It is exported so the operator install path can mirror
// these rules into its own ClusterRole — without that mirror the operator
// cannot create per-vcluster ClusterRoles (Kubernetes blocks privilege
// escalation: a subject cannot grant permissions it does not itself hold).
func SyncerClusterRoleRules() []rbacv1.PolicyRule {
	return []rbacv1.PolicyRule{
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
	}
}

// BuildClusterRole returns the ClusterRole for the syncer.
func BuildClusterRole(opts BuilderOptions) *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name:   ClusterRoleName(opts.Name, opts.Namespace),
			Labels: opts.Labels,
		},
		Rules: SyncerClusterRoleRules(),
	}
}

// BuildClusterRoleBinding returns the ClusterRoleBinding for the syncer.
func BuildClusterRoleBinding(opts BuilderOptions) *rbacv1.ClusterRoleBinding {
	crName := ClusterRoleName(opts.Name, opts.Namespace)
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:   crName,
			Labels: opts.Labels,
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     crName,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      "vc-" + opts.Name,
				Namespace: opts.Namespace,
			},
		},
	}
}

// BuildRole returns the namespace-scoped Role for full access within the vcluster namespace.
func BuildRole(opts BuilderOptions) *rbacv1.Role {
	return &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "vc-" + opts.Name,
			Namespace: opts.Namespace,
			Labels:    opts.Labels,
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{"*"},
				Resources: []string{"*"},
				Verbs:     []string{"*"},
			},
		},
	}
}

// BuildRoleBinding returns the namespace-scoped RoleBinding.
func BuildRoleBinding(opts BuilderOptions) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "vc-" + opts.Name,
			Namespace: opts.Namespace,
			Labels:    opts.Labels,
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     "vc-" + opts.Name,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      "vc-" + opts.Name,
				Namespace: opts.Namespace,
			},
		},
	}
}

// BuildService returns the main service for the virtual cluster. The Service
// type is LoadBalancer when opts.ExposeType == "LoadBalancer", otherwise
// ClusterIP.
func BuildService(opts BuilderOptions) *corev1.Service {
	svcType := corev1.ServiceTypeClusterIP
	if opts.ExposeType == "LoadBalancer" {
		svcType = corev1.ServiceTypeLoadBalancer
	}
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      opts.Name,
			Namespace: opts.Namespace,
			Labels:    opts.Labels,
		},
		Spec: corev1.ServiceSpec{
			Type:     svcType,
			Selector: opts.Labels,
			Ports: []corev1.ServicePort{
				{
					Name:       "https",
					Port:       ServicePort,
					TargetPort: intstr.FromInt32(K3sPort),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}
}

// ResourceQuotaName returns the name of the ResourceQuota installed on a
// virtual cluster's host namespace.
func ResourceQuotaName(name string) string {
	return "vc-" + name + "-quota"
}

// BuildResourceQuota returns a ResourceQuota for the vcluster's host namespace
// based on opts.Resources. Returns nil when no limits are specified — callers
// should skip creating the object in that case.
//
// The mapping is:
//   - CPU     → requests.cpu and limits.cpu
//   - Memory  → requests.memory and limits.memory
//   - Storage → requests.storage
//   - Pods    → pods
//
// CPU/memory are pinned identically as request and limit so a workload's
// usable share is unambiguous (and so the LimitRange-supplied defaults — which
// set request==limit — round-trip cleanly through admission). The k3s control
// plane and syncer pods count against the budget; size accordingly.
func BuildResourceQuota(opts BuilderOptions) *corev1.ResourceQuota {
	if opts.Resources.IsEmpty() {
		return nil
	}
	hard := corev1.ResourceList{}
	if opts.Resources.CPU != "" {
		q := resource.MustParse(opts.Resources.CPU)
		hard[corev1.ResourceRequestsCPU] = q
		hard[corev1.ResourceLimitsCPU] = q
	}
	if opts.Resources.Memory != "" {
		q := resource.MustParse(opts.Resources.Memory)
		hard[corev1.ResourceRequestsMemory] = q
		hard[corev1.ResourceLimitsMemory] = q
	}
	if opts.Resources.Storage != "" {
		hard[corev1.ResourceRequestsStorage] = resource.MustParse(opts.Resources.Storage)
	}
	if opts.Resources.Pods > 0 {
		hard[corev1.ResourcePods] = *resource.NewQuantity(int64(opts.Resources.Pods), resource.DecimalSI)
	}
	return &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ResourceQuotaName(opts.Name),
			Namespace: opts.Namespace,
			Labels:    opts.Labels,
		},
		Spec: corev1.ResourceQuotaSpec{
			Hard: hard,
		},
	}
}

// LimitRangeName returns the name of the LimitRange installed alongside the
// ResourceQuota for a virtual cluster's host namespace.
func LimitRangeName(name string) string {
	return "vc-" + name + "-limits"
}

// BuildLimitRange returns a LimitRange that supplies default container
// requests and limits in the vcluster's host namespace, so workloads created
// inside the vcluster without explicit resources don't get rejected by the
// ResourceQuota. Returns nil when no quota is installed.
func BuildLimitRange(opts BuilderOptions) *corev1.LimitRange {
	if opts.Resources.IsEmpty() {
		return nil
	}
	// A ResourceQuota that constrains requests.cpu/limits.cpu (or the memory
	// equivalents) requires every pod admitted to the namespace to declare
	// the corresponding request and limit. The LimitRange backstops that by
	// supplying defaults for any container that omits them.
	def := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("100m"),
		corev1.ResourceMemory: resource.MustParse("128Mi"),
	}
	defReq := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("100m"),
		corev1.ResourceMemory: resource.MustParse("128Mi"),
	}
	return &corev1.LimitRange{
		ObjectMeta: metav1.ObjectMeta{
			Name:      LimitRangeName(opts.Name),
			Namespace: opts.Namespace,
			Labels:    opts.Labels,
		},
		Spec: corev1.LimitRangeSpec{
			Limits: []corev1.LimitRangeItem{
				{
					Type:           corev1.LimitTypeContainer,
					Default:        def,
					DefaultRequest: defReq,
				},
			},
		},
	}
}

// BuildHeadlessService returns the headless service for the StatefulSet.
func BuildHeadlessService(opts BuilderOptions) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      opts.Name + "-headless",
			Namespace: opts.Namespace,
			Labels:    opts.Labels,
		},
		Spec: corev1.ServiceSpec{
			Type:      corev1.ServiceTypeClusterIP,
			ClusterIP: "None",
			Selector:  opts.Labels,
			Ports: []corev1.ServicePort{
				{
					Name:       "https",
					Port:       ServicePort,
					TargetPort: intstr.FromInt32(K3sPort),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}
}

// k3sArgs returns the command-line args for the k3s server container.
// It is exported via BuildStatefulSet — kept separate so the expose host
// can be appended to the TLS-SAN list when set.
func k3sArgs(opts BuilderOptions) []string {
	args := []string{
		"server",
		// coredns is disabled because the virtual cluster has no kubelet
		// (--disable-agent) and no CNI (--flannel-backend=none), so the
		// coredns Deployment k3s ships would never schedule. The syncer
		// skips kube-system and cannot translate it. See issue #5.
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
		"--tls-san=" + opts.Name + "." + opts.Namespace + ".svc.cluster.local",
		"--tls-san=" + opts.Name + "." + opts.Namespace + ".svc",
		"--tls-san=" + opts.Name,
	}
	if opts.ExposeHost != "" {
		args = append(args, "--tls-san="+opts.ExposeHost)
	}
	return args
}

// BuildStatefulSet returns the StatefulSet for the virtual cluster (k3s + syncer).
func BuildStatefulSet(opts BuilderOptions) *appsv1.StatefulSet {
	k3sImage := opts.K3sImage
	if k3sImage == "" {
		k3sImage = K3sImage
	}
	syncerImage := opts.SyncerImage
	if syncerImage == "" {
		syncerImage = SyncerImage
	}
	storage := opts.Storage
	if storage == "" {
		storage = "5Gi"
	}

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      opts.Name,
			Namespace: opts.Namespace,
			Labels:    opts.Labels,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    ptr.To[int32](1),
			ServiceName: opts.Name + "-headless",
			Selector: &metav1.LabelSelector{
				MatchLabels: opts.Labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: opts.Labels,
				},
				Spec: corev1.PodSpec{
					ServiceAccountName:           "vc-" + opts.Name,
					TerminationGracePeriodSeconds: ptr.To[int64](10),
					Containers: []corev1.Container{
						{
							Name:  "k3s",
							Image: k3sImage,
							Command: []string{
								"k3s",
							},
							Args: k3sArgs(opts),
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
									Value: opts.Name,
								},
								{
									// POD_IP is needed by the kubelet shim:
									// it's the bind address, the SAN baked
									// into its TLS cert, and the value the
									// syncer patches into every synced
									// virtual node's InternalIP.
									Name: "POD_IP",
									ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{
											FieldPath: "status.podIP",
										},
									},
								},
							},
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
								corev1.ResourceStorage: resource.MustParse(storage),
							},
						},
					},
				},
			},
		},
	}

	if opts.ImagePullSecret != "" {
		sts.Spec.Template.Spec.ImagePullSecrets = []corev1.LocalObjectReference{
			{Name: opts.ImagePullSecret},
		}
	}

	return sts
}
