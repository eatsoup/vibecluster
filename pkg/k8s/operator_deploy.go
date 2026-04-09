package k8s

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
)

const (
	// OperatorNamespace is the namespace where the operator is deployed.
	OperatorNamespace = "vibecluster-system"
	// OperatorName is the name of the operator deployment and related resources.
	OperatorName = "vibecluster-operator"
	// OperatorImage is the default operator container image.
	OperatorImage = "ghcr.io/eatsoup/vibecluster/operator:latest"
	// CRDName is the name of the VirtualCluster CRD.
	CRDName = "virtualclusters.vibecluster.dev"
)

// OperatorInstallOptions holds options for installing the operator.
type OperatorInstallOptions struct {
	// Image overrides the default operator container image.
	Image string
}

// InstallOperator installs the vibecluster operator (CRD, RBAC, Deployment).
func InstallOperator(ctx context.Context, client kubernetes.Interface, restConfig *rest.Config, opts OperatorInstallOptions) error {
	image := opts.Image
	if image == "" {
		image = OperatorImage
	}

	labels := operatorLabels()

	// 1. Create the CRD
	fmt.Println("  Installing VirtualCluster CRD...")
	if err := installCRD(ctx, restConfig); err != nil {
		return fmt.Errorf("installing CRD: %w", err)
	}

	// 2. Create operator namespace
	fmt.Printf("  Creating namespace %s...\n", OperatorNamespace)
	if err := ensureNamespaceExists(ctx, client, OperatorNamespace, labels); err != nil {
		return fmt.Errorf("creating namespace: %w", err)
	}

	// 3. Create service account
	fmt.Println("  Creating service account...")
	if err := ensureOperatorServiceAccount(ctx, client, labels); err != nil {
		return fmt.Errorf("creating service account: %w", err)
	}

	// 4. Create RBAC
	fmt.Println("  Creating RBAC resources...")
	if err := ensureOperatorRBAC(ctx, client, labels); err != nil {
		return fmt.Errorf("creating RBAC: %w", err)
	}

	// 5. Create deployment
	fmt.Println("  Creating operator deployment...")
	if err := ensureOperatorDeployment(ctx, client, image, labels); err != nil {
		return fmt.Errorf("creating deployment: %w", err)
	}

	return nil
}

// UninstallOperator removes the vibecluster operator (Deployment, RBAC, namespace, CRD).
func UninstallOperator(ctx context.Context, client kubernetes.Interface, restConfig *rest.Config) error {
	// 1. Delete deployment
	fmt.Println("  Deleting operator deployment...")
	err := client.AppsV1().Deployments(OperatorNamespace).Delete(ctx, OperatorName, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("deleting deployment: %w", err)
	}

	// 2. Delete RBAC
	fmt.Println("  Deleting RBAC resources...")
	_ = client.RbacV1().ClusterRoleBindings().Delete(ctx, OperatorName, metav1.DeleteOptions{})
	_ = client.RbacV1().ClusterRoles().Delete(ctx, OperatorName, metav1.DeleteOptions{})

	// 3. Delete service account
	_ = client.CoreV1().ServiceAccounts(OperatorNamespace).Delete(ctx, OperatorName, metav1.DeleteOptions{})

	// 4. Delete namespace
	fmt.Printf("  Deleting namespace %s...\n", OperatorNamespace)
	err = client.CoreV1().Namespaces().Delete(ctx, OperatorNamespace, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("deleting namespace: %w", err)
	}

	// 5. Delete CRD
	fmt.Println("  Deleting VirtualCluster CRD...")
	if err := uninstallCRD(ctx, restConfig); err != nil {
		return fmt.Errorf("deleting CRD: %w", err)
	}

	return nil
}

func operatorLabels() map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":      OperatorName,
		"app.kubernetes.io/component": "operator",
		"app.kubernetes.io/managed-by": LabelManagedByValue,
	}
}

func ensureNamespaceExists(ctx context.Context, client kubernetes.Interface, name string, labels map[string]string) error {
	_, err := client.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		_, err = client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name:   name,
				Labels: labels,
			},
		}, metav1.CreateOptions{})
	}
	return err
}

func ensureOperatorServiceAccount(ctx context.Context, client kubernetes.Interface, labels map[string]string) error {
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      OperatorName,
			Namespace: OperatorNamespace,
			Labels:    labels,
		},
	}
	_, err := client.CoreV1().ServiceAccounts(OperatorNamespace).Get(ctx, OperatorName, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		_, err = client.CoreV1().ServiceAccounts(OperatorNamespace).Create(ctx, sa, metav1.CreateOptions{})
	}
	return err
}

func ensureOperatorRBAC(ctx context.Context, client kubernetes.Interface, labels map[string]string) error {
	cr := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name:   OperatorName,
			Labels: labels,
		},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{"vibecluster.dev"}, Resources: []string{"virtualclusters"}, Verbs: []string{"get", "list", "watch", "create", "update", "patch", "delete"}},
			{APIGroups: []string{"vibecluster.dev"}, Resources: []string{"virtualclusters/status"}, Verbs: []string{"get", "update", "patch"}},
			{APIGroups: []string{"vibecluster.dev"}, Resources: []string{"virtualclusters/finalizers"}, Verbs: []string{"update"}},
			{APIGroups: []string{""}, Resources: []string{"namespaces"}, Verbs: []string{"get", "list", "watch", "create", "update", "delete"}},
			{APIGroups: []string{""}, Resources: []string{"serviceaccounts"}, Verbs: []string{"get", "list", "watch", "create", "update", "delete"}},
			{APIGroups: []string{""}, Resources: []string{"services"}, Verbs: []string{"get", "list", "watch", "create", "update", "delete"}},
			{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get", "list", "watch", "create", "update", "delete"}},
			{APIGroups: []string{"apps"}, Resources: []string{"statefulsets"}, Verbs: []string{"get", "list", "watch", "create", "update", "delete"}},
			{APIGroups: []string{"rbac.authorization.k8s.io"}, Resources: []string{"clusterroles", "clusterrolebindings"}, Verbs: []string{"get", "list", "watch", "create", "update", "delete"}},
			{APIGroups: []string{"rbac.authorization.k8s.io"}, Resources: []string{"roles", "rolebindings"}, Verbs: []string{"get", "list", "watch", "create", "update", "delete"}},
			{APIGroups: []string{""}, Resources: []string{"events"}, Verbs: []string{"create", "patch"}},
			{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, Verbs: []string{"get", "list", "watch", "create", "update", "patch", "delete"}},
		},
	}

	if _, err := client.RbacV1().ClusterRoles().Get(ctx, OperatorName, metav1.GetOptions{}); errors.IsNotFound(err) {
		if _, err := client.RbacV1().ClusterRoles().Create(ctx, cr, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("creating ClusterRole: %w", err)
		}
	} else if err != nil {
		return err
	}

	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:   OperatorName,
			Labels: labels,
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     OperatorName,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      OperatorName,
				Namespace: OperatorNamespace,
			},
		},
	}

	if _, err := client.RbacV1().ClusterRoleBindings().Get(ctx, OperatorName, metav1.GetOptions{}); errors.IsNotFound(err) {
		if _, err := client.RbacV1().ClusterRoleBindings().Create(ctx, crb, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("creating ClusterRoleBinding: %w", err)
		}
	} else if err != nil {
		return err
	}

	return nil
}

func ensureOperatorDeployment(ctx context.Context, client kubernetes.Interface, image string, labels map[string]string) error {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      OperatorName,
			Namespace: OperatorNamespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](1),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app.kubernetes.io/name": OperatorName,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					ServiceAccountName:            OperatorName,
					TerminationGracePeriodSeconds: ptr.To[int64](10),
					Containers: []corev1.Container{
						{
							Name:  "operator",
							Image: image,
							Args:  []string{"--leader-elect"},
							Ports: []corev1.ContainerPort{
								{Name: "metrics", ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
								{Name: "health", ContainerPort: 8081, Protocol: corev1.ProtocolTCP},
							},
							LivenessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path: "/healthz",
										Port: intstr.FromString("health"),
									},
								},
								InitialDelaySeconds: 15,
								PeriodSeconds:       20,
							},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path: "/readyz",
										Port: intstr.FromString("health"),
									},
								},
								InitialDelaySeconds: 5,
								PeriodSeconds:       10,
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
							SecurityContext: &corev1.SecurityContext{
								AllowPrivilegeEscalation: ptr.To(false),
								ReadOnlyRootFilesystem:   ptr.To(true),
								RunAsNonRoot:             ptr.To(true),
							},
						},
					},
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: ptr.To(true),
					},
				},
			},
		},
	}

	existing, err := client.AppsV1().Deployments(OperatorNamespace).Get(ctx, OperatorName, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		_, err = client.AppsV1().Deployments(OperatorNamespace).Create(ctx, dep, metav1.CreateOptions{})
		return err
	} else if err != nil {
		return err
	}

	// Update image if changed
	if len(existing.Spec.Template.Spec.Containers) > 0 && existing.Spec.Template.Spec.Containers[0].Image != image {
		existing.Spec.Template.Spec.Containers[0].Image = image
		_, err = client.AppsV1().Deployments(OperatorNamespace).Update(ctx, existing, metav1.UpdateOptions{})
		return err
	}

	return nil
}

func installCRD(ctx context.Context, restConfig *rest.Config) error {
	extClient, err := apiextensionsclient.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("creating apiextensions client: %w", err)
	}

	crd := buildVirtualClusterCRD()

	_, err = extClient.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, CRDName, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		_, err = extClient.ApiextensionsV1().CustomResourceDefinitions().Create(ctx, crd, metav1.CreateOptions{})
		return err
	}
	return err
}

func uninstallCRD(ctx context.Context, restConfig *rest.Config) error {
	extClient, err := apiextensionsclient.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("creating apiextensions client: %w", err)
	}

	err = extClient.ApiextensionsV1().CustomResourceDefinitions().Delete(ctx, CRDName, metav1.DeleteOptions{})
	if errors.IsNotFound(err) {
		return nil
	}
	return err
}

func buildVirtualClusterCRD() *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: CRDName,
		},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "vibecluster.dev",
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Kind:       "VirtualCluster",
				ListKind:   "VirtualClusterList",
				Plural:     "virtualclusters",
				Singular:   "virtualcluster",
				ShortNames: []string{"vc"},
			},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{
					Name:    "v1alpha1",
					Served:  true,
					Storage: true,
					Subresources: &apiextensionsv1.CustomResourceSubresources{
						Status: &apiextensionsv1.CustomResourceSubresourceStatus{},
					},
					AdditionalPrinterColumns: []apiextensionsv1.CustomResourceColumnDefinition{
						{Name: "Phase", Type: "string", JSONPath: ".status.phase"},
						{Name: "Ready", Type: "boolean", JSONPath: ".status.ready"},
						{Name: "Namespace", Type: "string", JSONPath: ".status.namespace"},
						{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
					},
					Schema: &apiextensionsv1.CustomResourceValidation{
						OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
							Type:        "object",
							Description: "VirtualCluster is a lightweight virtual Kubernetes cluster running inside the host cluster.",
							Properties: map[string]apiextensionsv1.JSONSchemaProps{
								"apiVersion": {Type: "string"},
								"kind":       {Type: "string"},
								"metadata":   {Type: "object"},
								"spec": {
									Type: "object",
									Properties: map[string]apiextensionsv1.JSONSchemaProps{
										"k3sImage":    {Type: "string", Default: jsonRaw(`"rancher/k3s:v1.28.5-k3s1"`)},
										"syncerImage": {Type: "string", Default: jsonRaw(`"ghcr.io/eatsoup/vibecluster/syncer:latest"`)},
										"storage":     {Type: "string", Default: jsonRaw(`"5Gi"`)},
									},
								},
								"status": {
									Type: "object",
									Properties: map[string]apiextensionsv1.JSONSchemaProps{
										"phase":              {Type: "string"},
										"ready":              {Type: "boolean"},
										"message":            {Type: "string"},
										"namespace":          {Type: "string"},
										"observedGeneration": {Type: "integer", Format: "int64"},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

func jsonRaw(s string) *apiextensionsv1.JSON {
	return &apiextensionsv1.JSON{Raw: []byte(s)}
}
