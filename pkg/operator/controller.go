package operator

import (
	"context"
	"fmt"
	"time"

	vibeclusterv1alpha1 "github.com/eatsoup/vibecluster/api/v1alpha1"
	"github.com/eatsoup/vibecluster/pkg/k8s"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
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
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles;clusterrolebindings,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;delete

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

	// 1. Ensure namespace
	if err := r.ensureNamespace(ctx, opts); err != nil {
		return r.updateStatus(ctx, &vc, vibeclusterv1alpha1.VirtualClusterPhaseFailed, false, fmt.Sprintf("Failed to create namespace: %v", err), opts.Namespace)
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

	// 6. Check StatefulSet readiness
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

// ensureNamespace creates the namespace if it doesn't exist.
func (r *VirtualClusterReconciler) ensureNamespace(ctx context.Context, opts k8s.BuilderOptions) error {
	ns := k8s.BuildNamespace(opts, map[string]string{
		k8s.AnnotationCreated: time.Now().UTC().Format(time.RFC3339),
	})

	existing := &corev1.Namespace{}
	err := r.Get(ctx, types.NamespacedName{Name: ns.Name}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, ns)
	}
	return err
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
func (r *VirtualClusterReconciler) ensureServices(ctx context.Context, opts k8s.BuilderOptions) error {
	// Main service
	svc := k8s.BuildService(opts)
	existing := &corev1.Service{}
	if err := r.Get(ctx, types.NamespacedName{Name: svc.Name, Namespace: svc.Namespace}, existing); errors.IsNotFound(err) {
		if err := r.Create(ctx, svc); err != nil {
			return fmt.Errorf("creating service: %w", err)
		}
	} else if err != nil {
		return err
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

// ensureStatefulSet creates or updates the StatefulSet.
func (r *VirtualClusterReconciler) ensureStatefulSet(ctx context.Context, opts k8s.BuilderOptions) error {
	desired := k8s.BuildStatefulSet(opts)
	existing := &appsv1.StatefulSet{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	} else if err != nil {
		return err
	}

	// Update container images if changed
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
