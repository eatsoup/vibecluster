package operator

import (
	"context"
	"fmt"
	"time"

	vibeclusterv1alpha1 "github.com/eatsoup/vibecluster/api/v1alpha1"
	"github.com/eatsoup/vibecluster/pkg/k8s"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// Finalizer is the finalizer added to VirtualCluster resources.
	Finalizer = "vibecluster.dev/finalizer"
)

// VirtualClusterReconciler reconciles a VirtualCluster object.
type VirtualClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=vibecluster.dev,resources=virtualclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=vibecluster.dev,resources=virtualclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=vibecluster.dev,resources=virtualclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups="",resources=resourcequotas,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=limitranges,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles;clusterrolebindings,verbs=get;list;watch;create;update;delete;bind;escalate
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;delete;bind;escalate

// Reconcile handles the reconciliation loop for VirtualCluster resources.
func (r *VirtualClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the VirtualCluster instance
	var vc vibeclusterv1alpha1.VirtualCluster
	if err := r.Get(ctx, req.NamespacedName, &vc); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !vc.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &vc)
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(&vc, Finalizer) {
		controllerutil.AddFinalizer(&vc, Finalizer)
		if err := r.Update(ctx, &vc); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Build options from the CR spec, applying defaults
	opts := r.buildOptions(&vc)

	// Update status to Pending if not yet set
	if vc.Status.Phase == "" {
		return r.updateStatus(ctx, &vc, vibeclusterv1alpha1.VirtualClusterPhasePending, false, "Reconciling virtual cluster resources", opts.Namespace)
	}

	logger.Info("Reconciling virtual cluster", "name", vc.Name, "namespace", opts.Namespace)

	// 1. Ensure namespace (may allocate vnode CIDRs and store them on opts)
	if err := r.ensureNamespace(ctx, &opts); err != nil {
		return r.updateStatus(ctx, &vc, vibeclusterv1alpha1.VirtualClusterPhaseFailed, false, fmt.Sprintf("Failed to create namespace: %v", err), opts.Namespace)
	}

	// 1a. Ensure ResourceQuota + LimitRange. These must exist *before* the
	//     StatefulSet so the LimitRange's defaults apply at admission time
	//     for the k3s/syncer pods — without that, the quota would reject
	//     them for having no requests/limits set on every container.
	if err := r.ensureResourceLimits(ctx, opts); err != nil {
		return r.updateStatus(ctx, &vc, vibeclusterv1alpha1.VirtualClusterPhaseFailed, false, fmt.Sprintf("Failed to apply resource limits: %v", err), opts.Namespace)
	}

	// 2. Ensure service account
	if err := r.ensureServiceAccount(ctx, opts); err != nil {
		return r.updateStatus(ctx, &vc, vibeclusterv1alpha1.VirtualClusterPhaseFailed, false, fmt.Sprintf("Failed to create service account: %v", err), opts.Namespace)
	}

	// 3. Ensure RBAC
	if err := r.ensureRBAC(ctx, opts); err != nil {
		return r.updateStatus(ctx, &vc, vibeclusterv1alpha1.VirtualClusterPhaseFailed, false, fmt.Sprintf("Failed to create RBAC: %v", err), opts.Namespace)
	}

	// 4. Ensure services
	if err := r.ensureServices(ctx, opts); err != nil {
		return r.updateStatus(ctx, &vc, vibeclusterv1alpha1.VirtualClusterPhaseFailed, false, fmt.Sprintf("Failed to create services: %v", err), opts.Namespace)
	}

	// 5. Ensure StatefulSet
	if err := r.ensureStatefulSet(ctx, opts); err != nil {
		return r.updateStatus(ctx, &vc, vibeclusterv1alpha1.VirtualClusterPhaseFailed, false, fmt.Sprintf("Failed to create StatefulSet: %v", err), opts.Namespace)
	}

	// 6. Ensure Ingress (if expose.type == Ingress)
	if err := r.ensureIngress(ctx, opts); err != nil {
		return r.updateStatus(ctx, &vc, vibeclusterv1alpha1.VirtualClusterPhaseFailed, false, fmt.Sprintf("Failed to create Ingress: %v", err), opts.Namespace)
	}

	// 6a. Ensure vnode agent Deployment (if vnode mode)
	if err := r.ensureVNodeDeployment(ctx, &opts); err != nil {
		return r.updateStatus(ctx, &vc, vibeclusterv1alpha1.VirtualClusterPhaseFailed, false, fmt.Sprintf("Failed to create vnode Deployment: %v", err), opts.Namespace)
	}

	// 7. Check StatefulSet readiness
	ready, msg := r.checkReadiness(ctx, opts)
	if ready {
		return r.updateStatus(ctx, &vc, vibeclusterv1alpha1.VirtualClusterPhaseRunning, true, "Virtual cluster is running", opts.Namespace)
	}

	// Not ready yet — requeue
	result, err := r.updateStatus(ctx, &vc, vibeclusterv1alpha1.VirtualClusterPhasePending, false, msg, opts.Namespace)
	if err != nil {
		return result, err
	}
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// buildOptions creates BuilderOptions from the VirtualCluster CR spec.
func (r *VirtualClusterReconciler) buildOptions(vc *vibeclusterv1alpha1.VirtualCluster) k8s.BuilderOptions {
	opts := k8s.DefaultBuilderOptions(vc.Name)

	if vc.Spec.K3sImage != "" {
		opts.K3sImage = vc.Spec.K3sImage
	}
	if vc.Spec.SyncerImage != "" {
		opts.SyncerImage = vc.Spec.SyncerImage
	}
	if vc.Spec.Storage != "" {
		opts.Storage = vc.Spec.Storage
	}
	if vc.Spec.Expose != nil {
		opts.ExposeType = string(vc.Spec.Expose.Type)
		opts.ExposeHost = vc.Spec.Expose.Host
		opts.ExposeIngressClass = vc.Spec.Expose.IngressClass
	}
	if vc.Spec.Resources != nil {
		opts.Resources = &k8s.ResourceLimits{
			CPU:     vc.Spec.Resources.CPU,
			Memory:  vc.Spec.Resources.Memory,
			Storage: vc.Spec.Resources.Storage,
			Pods:    vc.Spec.Resources.Pods,
		}
	}
	opts.VNode = vc.Spec.VNode

	return opts
}

// updateStatus updates the VirtualCluster status subresource.
func (r *VirtualClusterReconciler) updateStatus(ctx context.Context, vc *vibeclusterv1alpha1.VirtualCluster, phase vibeclusterv1alpha1.VirtualClusterPhase, ready bool, message, namespace string) (ctrl.Result, error) {
	vc.Status.Phase = phase
	vc.Status.Ready = ready
	vc.Status.Message = message
	vc.Status.Namespace = namespace
	vc.Status.ObservedGeneration = vc.Generation

	if err := r.Status().Update(ctx, vc); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// reconcileDelete handles cleanup when a VirtualCluster is being deleted.
func (r *VirtualClusterReconciler) reconcileDelete(ctx context.Context, vc *vibeclusterv1alpha1.VirtualCluster) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Deleting virtual cluster resources", "name", vc.Name)

	opts := r.buildOptions(vc)

	// Delete cluster-scoped RBAC (not garbage-collected by namespace deletion)
	crName := k8s.ClusterRoleName(opts.Name, opts.Namespace)

	crb := &rbacv1.ClusterRoleBinding{}
	if err := r.Get(ctx, types.NamespacedName{Name: crName}, crb); err == nil {
		_ = r.Delete(ctx, crb)
	}

	cr := &rbacv1.ClusterRole{}
	if err := r.Get(ctx, types.NamespacedName{Name: crName}, cr); err == nil {
		_ = r.Delete(ctx, cr)
	}

	// Delete the namespace (cascades to all namespaced resources)
	ns := &corev1.Namespace{}
	if err := r.Get(ctx, types.NamespacedName{Name: opts.Namespace}, ns); err == nil {
		if err := r.Delete(ctx, ns); err != nil && !errors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("deleting namespace: %w", err)
		}
	}

	// Remove finalizer
	controllerutil.RemoveFinalizer(vc, Finalizer)
	if err := r.Update(ctx, vc); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("Virtual cluster resources deleted", "name", vc.Name)
	return ctrl.Result{}, nil
}

// ensureNamespace creates the namespace if it doesn't exist. When vnode mode
// is enabled, it allocates pod/service CIDRs and records them as namespace
// annotations so the CIDR allocator can avoid collisions.
func (r *VirtualClusterReconciler) ensureNamespace(ctx context.Context, opts *k8s.BuilderOptions) error {
	annotations := map[string]string{
		k8s.AnnotationCreated: time.Now().UTC().Format(time.RFC3339),
	}

	existing := &corev1.Namespace{}
	err := r.Get(ctx, types.NamespacedName{Name: opts.Namespace}, existing)
	if errors.IsNotFound(err) {
		// Allocate CIDRs before creating the namespace so the annotations
		// are present from the start.
		if opts.VNode {
			cidrs, allocErr := r.allocateVNodeCIDRs(ctx, opts)
			if allocErr != nil {
				return allocErr
			}
			annotations[k8s.AnnotationPodCIDR] = cidrs.Pod
			annotations[k8s.AnnotationServiceCIDR] = cidrs.Service
		}
		ns := k8s.BuildNamespace(*opts, annotations)
		return r.Create(ctx, ns)
	}
	if err != nil {
		return err
	}

	// Namespace already exists — pick up existing CIDR annotations or allocate.
	if opts.VNode {
		podCIDR := existing.Annotations[k8s.AnnotationPodCIDR]
		svcCIDR := existing.Annotations[k8s.AnnotationServiceCIDR]
		if podCIDR != "" && svcCIDR != "" {
			opts.VNodeCIDRs = k8s.VNodeCIDRsFromAnnotations(podCIDR, svcCIDR)
		} else {
			cidrs, allocErr := r.allocateVNodeCIDRs(ctx, opts)
			if allocErr != nil {
				return allocErr
			}
			if existing.Annotations == nil {
				existing.Annotations = map[string]string{}
			}
			existing.Annotations[k8s.AnnotationPodCIDR] = cidrs.Pod
			existing.Annotations[k8s.AnnotationServiceCIDR] = cidrs.Service
			if err := r.Update(ctx, existing); err != nil {
				return fmt.Errorf("annotating namespace with CIDRs: %w", err)
			}
		}
	}
	return nil
}

// allocateVNodeCIDRs picks a free CIDR pair and stores it on opts.
func (r *VirtualClusterReconciler) allocateVNodeCIDRs(ctx context.Context, opts *k8s.BuilderOptions) (k8s.VNodeCIDRs, error) {
	nss := &corev1.NamespaceList{}
	if err := r.List(ctx, nss, client.MatchingLabels{k8s.LabelManagedBy: k8s.LabelManagedByValue}); err != nil {
		return k8s.VNodeCIDRs{}, fmt.Errorf("listing vibecluster namespaces: %w", err)
	}
	cidrs, err := k8s.PickFreeVNodeCIDRs(nss.Items)
	if err != nil {
		return k8s.VNodeCIDRs{}, err
	}
	opts.VNodeCIDRs = cidrs
	return cidrs, nil
}

// ensureResourceLimits creates or updates the per-vcluster ResourceQuota and
// LimitRange when opts.Resources is set, and removes them when it isn't (so
// clearing spec.resources actually relaxes the budget).
func (r *VirtualClusterReconciler) ensureResourceLimits(ctx context.Context, opts k8s.BuilderOptions) error {
	desiredQuota := k8s.BuildResourceQuota(opts)
	desiredLR := k8s.BuildLimitRange(opts)

	// ResourceQuota
	existingQuota := &corev1.ResourceQuota{}
	err := r.Get(ctx, types.NamespacedName{Name: k8s.ResourceQuotaName(opts.Name), Namespace: opts.Namespace}, existingQuota)
	switch {
	case errors.IsNotFound(err):
		if desiredQuota != nil {
			if err := r.Create(ctx, desiredQuota); err != nil {
				return fmt.Errorf("creating ResourceQuota: %w", err)
			}
		}
	case err != nil:
		return err
	default:
		if desiredQuota == nil {
			if err := r.Delete(ctx, existingQuota); err != nil && !errors.IsNotFound(err) {
				return fmt.Errorf("deleting stale ResourceQuota: %w", err)
			}
		} else {
			existingQuota.Spec = desiredQuota.Spec
			if err := r.Update(ctx, existingQuota); err != nil {
				return fmt.Errorf("updating ResourceQuota: %w", err)
			}
		}
	}

	// LimitRange
	existingLR := &corev1.LimitRange{}
	err = r.Get(ctx, types.NamespacedName{Name: k8s.LimitRangeName(opts.Name), Namespace: opts.Namespace}, existingLR)
	switch {
	case errors.IsNotFound(err):
		if desiredLR != nil {
			if err := r.Create(ctx, desiredLR); err != nil {
				return fmt.Errorf("creating LimitRange: %w", err)
			}
		}
	case err != nil:
		return err
	default:
		if desiredLR == nil {
			if err := r.Delete(ctx, existingLR); err != nil && !errors.IsNotFound(err) {
				return fmt.Errorf("deleting stale LimitRange: %w", err)
			}
		} else {
			existingLR.Spec = desiredLR.Spec
			if err := r.Update(ctx, existingLR); err != nil {
				return fmt.Errorf("updating LimitRange: %w", err)
			}
		}
	}

	return nil
}

// ensureServiceAccount creates the service account if it doesn't exist.
func (r *VirtualClusterReconciler) ensureServiceAccount(ctx context.Context, opts k8s.BuilderOptions) error {
	sa := k8s.BuildServiceAccount(opts)
	existing := &corev1.ServiceAccount{}
	err := r.Get(ctx, types.NamespacedName{Name: sa.Name, Namespace: sa.Namespace}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, sa)
	}
	return err
}

// ensureRBAC creates all RBAC resources if they don't exist.
func (r *VirtualClusterReconciler) ensureRBAC(ctx context.Context, opts k8s.BuilderOptions) error {
	// ClusterRole
	cr := k8s.BuildClusterRole(opts)
	existingCR := &rbacv1.ClusterRole{}
	if err := r.Get(ctx, types.NamespacedName{Name: cr.Name}, existingCR); errors.IsNotFound(err) {
		if err := r.Create(ctx, cr); err != nil {
			return fmt.Errorf("creating ClusterRole: %w", err)
		}
	} else if err != nil {
		return err
	}

	// ClusterRoleBinding
	crb := k8s.BuildClusterRoleBinding(opts)
	existingCRB := &rbacv1.ClusterRoleBinding{}
	if err := r.Get(ctx, types.NamespacedName{Name: crb.Name}, existingCRB); errors.IsNotFound(err) {
		if err := r.Create(ctx, crb); err != nil {
			return fmt.Errorf("creating ClusterRoleBinding: %w", err)
		}
	} else if err != nil {
		return err
	}

	// Role
	role := k8s.BuildRole(opts)
	existingRole := &rbacv1.Role{}
	if err := r.Get(ctx, types.NamespacedName{Name: role.Name, Namespace: role.Namespace}, existingRole); errors.IsNotFound(err) {
		if err := r.Create(ctx, role); err != nil {
			return fmt.Errorf("creating Role: %w", err)
		}
	} else if err != nil {
		return err
	}

	// RoleBinding
	rb := k8s.BuildRoleBinding(opts)
	existingRB := &rbacv1.RoleBinding{}
	if err := r.Get(ctx, types.NamespacedName{Name: rb.Name, Namespace: rb.Namespace}, existingRB); errors.IsNotFound(err) {
		if err := r.Create(ctx, rb); err != nil {
			return fmt.Errorf("creating RoleBinding: %w", err)
		}
	} else if err != nil {
		return err
	}

	return nil
}

// ensureServices creates the main and headless services if they don't exist.
// Reconciles the main Service type with the desired ExposeType so flipping
// `spec.expose.type` between LoadBalancer and ClusterIP/Ingress takes effect.
func (r *VirtualClusterReconciler) ensureServices(ctx context.Context, opts k8s.BuilderOptions) error {
	// Main service
	svc := k8s.BuildService(opts)
	existing := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: svc.Name, Namespace: svc.Namespace}, existing)
	if errors.IsNotFound(err) {
		if err := r.Create(ctx, svc); err != nil {
			return fmt.Errorf("creating service: %w", err)
		}
	} else if err != nil {
		return err
	} else if existing.Spec.Type != svc.Spec.Type {
		// Type change requires clearing fields incompatible with the new type.
		existing.Spec.Type = svc.Spec.Type
		if svc.Spec.Type == corev1.ServiceTypeClusterIP {
			existing.Spec.Ports = svc.Spec.Ports
		}
		if err := r.Update(ctx, existing); err != nil {
			return fmt.Errorf("updating service type: %w", err)
		}
	}

	// Headless service
	headless := k8s.BuildHeadlessService(opts)
	existingHeadless := &corev1.Service{}
	if err := r.Get(ctx, types.NamespacedName{Name: headless.Name, Namespace: headless.Namespace}, existingHeadless); errors.IsNotFound(err) {
		if err := r.Create(ctx, headless); err != nil {
			return fmt.Errorf("creating headless service: %w", err)
		}
	} else if err != nil {
		return err
	}

	return nil
}

// ensureIngress reconciles the Ingress for the virtual cluster API. When
// ExposeType is "Ingress" the Ingress is created or updated; for any other
// ExposeType (or none) any pre-existing Ingress is removed.
func (r *VirtualClusterReconciler) ensureIngress(ctx context.Context, opts k8s.BuilderOptions) error {
	if opts.ExposeType != "Ingress" {
		// Clean up if a previous reconcile created one.
		existing := &networkingv1.Ingress{}
		err := r.Get(ctx, types.NamespacedName{Name: opts.Name, Namespace: opts.Namespace}, existing)
		if errors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := r.Delete(ctx, existing); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("deleting stale ingress: %w", err)
		}
		return nil
	}

	if opts.ExposeHost == "" {
		return fmt.Errorf("expose.host is required when expose.type is Ingress")
	}

	desired := k8s.BuildIngress(opts.Name, opts.Namespace, opts.Labels, opts.ExposeHost, opts.ExposeIngressClass)
	existing := &networkingv1.Ingress{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if errors.IsNotFound(err) {
		if err := r.Create(ctx, desired); err != nil {
			return fmt.Errorf("creating ingress: %w", err)
		}
		return nil
	}
	if err != nil {
		return err
	}
	existing.Spec = desired.Spec
	if existing.Annotations == nil {
		existing.Annotations = map[string]string{}
	}
	for k, v := range desired.Annotations {
		existing.Annotations[k] = v
	}
	if err := r.Update(ctx, existing); err != nil {
		return fmt.Errorf("updating ingress: %w", err)
	}
	return nil
}

// ensureVNodeDeployment creates or updates the vnode agent Deployment when
// vnode mode is enabled. When vnode is disabled, any pre-existing vnode
// Deployment is removed.
func (r *VirtualClusterReconciler) ensureVNodeDeployment(ctx context.Context, opts *k8s.BuilderOptions) error {
	depName := opts.Name + "-vnode"

	if !opts.VNode {
		// Clean up if a previous reconcile created one.
		existing := &appsv1.Deployment{}
		err := r.Get(ctx, types.NamespacedName{Name: depName, Namespace: opts.Namespace}, existing)
		if errors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := r.Delete(ctx, existing); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("deleting stale vnode deployment: %w", err)
		}
		return nil
	}

	desired := k8s.BuildVNodeDeployment(*opts)
	existing := &appsv1.Deployment{}
	err := r.Get(ctx, types.NamespacedName{Name: depName, Namespace: opts.Namespace}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	// Reconcile the agent image.
	needsUpdate := false
	for i, c := range existing.Spec.Template.Spec.Containers {
		for _, dc := range desired.Spec.Template.Spec.Containers {
			if c.Name == dc.Name && c.Image != dc.Image {
				existing.Spec.Template.Spec.Containers[i].Image = dc.Image
				needsUpdate = true
			}
		}
	}
	if needsUpdate {
		return r.Update(ctx, existing)
	}
	return nil
}

// ensureStatefulSet creates or updates the StatefulSet. Container images and
// the k3s args (which carry the TLS-SAN list — including the expose host)
// are reconciled in place.
func (r *VirtualClusterReconciler) ensureStatefulSet(ctx context.Context, opts k8s.BuilderOptions) error {
	desired := k8s.BuildStatefulSet(opts)
	existing := &appsv1.StatefulSet{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	} else if err != nil {
		return err
	}

	needsUpdate := false
	for i, c := range existing.Spec.Template.Spec.Containers {
		for _, dc := range desired.Spec.Template.Spec.Containers {
			if c.Name != dc.Name {
				continue
			}
			if c.Image != dc.Image {
				existing.Spec.Template.Spec.Containers[i].Image = dc.Image
				needsUpdate = true
			}
			// Reconcile k3s args so the expose host appears in TLS-SAN.
			if c.Name == "k3s" && !stringSlicesEqual(c.Args, dc.Args) {
				existing.Spec.Template.Spec.Containers[i].Args = dc.Args
				needsUpdate = true
			}
		}
	}

	if needsUpdate {
		return r.Update(ctx, existing)
	}

	return nil
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// checkReadiness checks if the StatefulSet has ready replicas.
func (r *VirtualClusterReconciler) checkReadiness(ctx context.Context, opts k8s.BuilderOptions) (bool, string) {
	sts := &appsv1.StatefulSet{}
	err := r.Get(ctx, types.NamespacedName{Name: opts.Name, Namespace: opts.Namespace}, sts)
	if err != nil {
		return false, fmt.Sprintf("Checking readiness: %v", err)
	}

	if sts.Status.ReadyReplicas > 0 {
		return true, "Virtual cluster is running"
	}

	return false, fmt.Sprintf("Waiting for StatefulSet to be ready (%d/%d replicas)",
		sts.Status.ReadyReplicas, *sts.Spec.Replicas)
}

// SetupWithManager sets up the controller with the Manager.
func (r *VirtualClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&vibeclusterv1alpha1.VirtualCluster{}).
		Owns(&appsv1.StatefulSet{}).
		Complete(r)
}
