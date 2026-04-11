package syncer

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/eatsoup/vibecluster/pkg/k8s"
	"github.com/eatsoup/vibecluster/pkg/syncer/kubeletshim"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	// LabelSyncedFrom marks a resource as synced from a virtual cluster.
	LabelSyncedFrom = "vibecluster.dev/synced-from"
	// LabelVirtualName stores the original name in the virtual cluster.
	LabelVirtualName = "vibecluster.dev/virtual-name"
	// LabelVirtualNamespace stores the original namespace in the virtual cluster.
	LabelVirtualNamespace = "vibecluster.dev/virtual-namespace"
)

// Syncer syncs resources between a virtual cluster and the host cluster.
type Syncer struct {
	name       string
	hostClient kubernetes.Interface
	vClient    kubernetes.Interface
	hostNS     string

	// Optional kubelet shim configuration. When ShimPodIP is non-empty the
	// syncer (a) starts the kubelet shim during Run and (b) overrides
	// every synced virtual node's InternalIP and kubeletEndpoint.port to
	// point at the shim, so logs/exec/portforward go through it instead of
	// hitting the host kubelet directly. See pkg/syncer/kubeletshim for
	// the rationale.
	ShimHostConfig *rest.Config
	ShimPodIP      string
	ShimPort       int32
	ShimCACertPath string
	ShimCAKeyPath  string
}

// New creates a new Syncer.
func New(name string, hostClient, vClient kubernetes.Interface) *Syncer {
	return &Syncer{
		name:       name,
		hostClient: hostClient,
		vClient:    vClient,
		hostNS:     k8s.NamespaceName(name),
	}
}

// Run starts all sync loops. Blocks until context is cancelled.
func (s *Syncer) Run(ctx context.Context) error {
	fmt.Printf("Starting syncer for virtual cluster %q\n", s.name)
	fmt.Printf("  Host namespace: %s\n", s.hostNS)
	fmt.Printf("  Syncing: pods, services, configmaps, secrets\n")

	var wg sync.WaitGroup
	errCh := make(chan error, 6)

	syncFuncs := []struct {
		name string
		fn   func(context.Context) error
	}{
		{"pods", s.syncPods},
		{"services", s.syncServices},
		{"configmaps", s.syncConfigMaps},
		{"secrets", s.syncSecrets},
	}

	for _, sf := range syncFuncs {
		wg.Add(1)
		go func(name string, fn func(context.Context) error) {
			defer wg.Done()
			fmt.Printf("  Starting %s syncer\n", name)
			if err := fn(ctx); err != nil && ctx.Err() == nil {
				errCh <- fmt.Errorf("%s syncer: %w", name, err)
			}
		}(sf.name, sf.fn)
	}

	// Also sync nodes from host to virtual
	wg.Add(1)
	go func() {
		defer wg.Done()
		fmt.Printf("  Starting nodes syncer (host -> virtual)\n")
		if err := s.syncNodes(ctx); err != nil && ctx.Err() == nil {
			errCh <- fmt.Errorf("nodes syncer: %w", err)
		}
	}()

	// Watch host pods so we can reflect their status (and bind nodeName)
	// back into the virtual cluster.
	wg.Add(1)
	go func() {
		defer wg.Done()
		fmt.Printf("  Starting host pods syncer (host -> virtual status)\n")
		if err := s.syncHostPods(ctx); err != nil && ctx.Err() == nil {
			errCh <- fmt.Errorf("host pods syncer: %w", err)
		}
	}()

	// Start the kubelet shim if configured. This serves the kubelet API
	// (logs/exec/attach/portforward) on ShimPort and proxies each request
	// to the matching pod subresource on the host kube-apiserver, after
	// translating virtual pod (name, namespace) to the host pod name.
	if s.ShimHostConfig != nil {
		shim, err := kubeletshim.New(kubeletshim.Config{
			HostConfig:    s.ShimHostConfig,
			HostNamespace: s.hostNS,
			TranslateName: s.HostName,
			PodIP:         s.ShimPodIP,
			Port:          int(s.ShimPort),
			CACertPath:    s.ShimCACertPath,
			CAKeyPath:     s.ShimCAKeyPath,
		})
		if err != nil {
			return fmt.Errorf("creating kubelet shim: %w", err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			fmt.Printf("  Starting kubelet shim on :%d (PodIP=%s)\n", s.ShimPort, s.ShimPodIP)
			if err := shim.Run(ctx); err != nil && ctx.Err() == nil {
				errCh <- fmt.Errorf("kubelet shim: %w", err)
			}
		}()
	}

	wg.Wait()
	close(errCh)

	var errs []string
	for err := range errCh {
		errs = append(errs, err.Error())
	}
	if len(errs) > 0 {
		return fmt.Errorf("syncer errors: %s", strings.Join(errs, "; "))
	}
	return nil
}

// HostName translates a virtual cluster resource name to its host cluster name.
// Format: <vcluster-name>-x-<name>-x-<namespace>
func (s *Syncer) HostName(name, namespace string) string {
	return fmt.Sprintf("%s-x-%s-x-%s", s.name, name, namespace)
}

// syncLabels returns labels for a synced resource on the host.
func (s *Syncer) syncLabels(virtualName, virtualNamespace string) map[string]string {
	labels := k8s.Labels(s.name)
	labels[LabelSyncedFrom] = s.name
	labels[LabelVirtualName] = virtualName
	labels[LabelVirtualNamespace] = virtualNamespace
	return labels
}

// isSystemNamespace returns true for namespaces we should not sync from.
func isSystemNamespace(ns string) bool {
	return ns == "kube-system" || ns == "kube-public" || ns == "kube-node-lease"
}

// isSystemSecret returns true for secrets we should skip syncing.
func isSystemSecret(secret *corev1.Secret) bool {
	if secret.Type == corev1.SecretTypeServiceAccountToken {
		return true
	}
	// Skip k3s internal secrets
	if strings.HasPrefix(secret.Name, "k3s-") {
		return true
	}
	return false
}

// watchWithRetry wraps a watch function with retry logic.
func watchWithRetry(ctx context.Context, name string, fn func(ctx context.Context) error) error {
	for {
		if err := fn(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			fmt.Printf("  [%s] watch error, retrying in 5s: %v\n", name, err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(5 * time.Second):
			}
			continue
		}
		return nil
	}
}

// syncPods watches pods in the virtual cluster and syncs them to the host.
func (s *Syncer) syncPods(ctx context.Context) error {
	return watchWithRetry(ctx, "pods", func(ctx context.Context) error {
		watcher, err := s.vClient.CoreV1().Pods("").Watch(ctx, metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("watching pods: %w", err)
		}
		defer watcher.Stop()

		for {
			select {
			case <-ctx.Done():
				return nil
			case event, ok := <-watcher.ResultChan():
				if !ok {
					return fmt.Errorf("watch channel closed")
				}
				pod, ok := event.Object.(*corev1.Pod)
				if !ok || isSystemNamespace(pod.Namespace) {
					continue
				}

				switch event.Type {
				case watch.Added, watch.Modified:
					if err := s.syncPodToHost(ctx, pod); err != nil {
						fmt.Printf("  [pods] sync error for %s/%s: %v\n", pod.Namespace, pod.Name, err)
					}
				case watch.Deleted:
					hostName := s.HostName(pod.Name, pod.Namespace)
					err := s.hostClient.CoreV1().Pods(s.hostNS).Delete(ctx, hostName, metav1.DeleteOptions{})
					if err != nil && !errors.IsNotFound(err) {
						fmt.Printf("  [pods] delete error for %s: %v\n", hostName, err)
					} else if err == nil {
						fmt.Printf("  [pods] deleted %s/%s -> %s/%s\n", pod.Namespace, pod.Name, s.hostNS, hostName)
					}
				}
			}
		}
	})
}

func (s *Syncer) syncPodToHost(ctx context.Context, vPod *corev1.Pod) error {
	hostName := s.HostName(vPod.Name, vPod.Namespace)
	labels := s.syncLabels(vPod.Name, vPod.Namespace)

	// Merge virtual pod labels
	for k, v := range vPod.Labels {
		if !strings.HasPrefix(k, "vibecluster.dev/") {
			labels[k] = v
		}
	}

	hostPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        hostName,
			Namespace:   s.hostNS,
			Labels:      labels,
			Annotations: vPod.Annotations,
		},
		Spec: *vPod.Spec.DeepCopy(),
	}

	// Clear fields that shouldn't be carried over
	hostPod.Spec.NodeName = ""
	hostPod.Spec.ServiceAccountName = "vc-" + s.name
	hostPod.Spec.AutomountServiceAccountToken = boolPtr(false)
	hostPod.Spec.DeprecatedServiceAccount = ""

	// Rewrite volume references
	for i := range hostPod.Spec.Volumes {
		vol := &hostPod.Spec.Volumes[i]
		if vol.ConfigMap != nil {
			vol.ConfigMap.Name = s.HostName(vol.ConfigMap.Name, vPod.Namespace)
		}
		if vol.Secret != nil {
			vol.Secret.SecretName = s.HostName(vol.Secret.SecretName, vPod.Namespace)
		}
	}

	// Remove service account token volumes
	var filteredVolumes []corev1.Volume
	for _, v := range hostPod.Spec.Volumes {
		if strings.Contains(v.Name, "kube-api-access") || strings.Contains(v.Name, "token") {
			continue
		}
		filteredVolumes = append(filteredVolumes, v)
	}
	hostPod.Spec.Volumes = filteredVolumes

	// Remove service account token volume mounts from containers
	for i := range hostPod.Spec.Containers {
		var mounts []corev1.VolumeMount
		for _, m := range hostPod.Spec.Containers[i].VolumeMounts {
			if strings.Contains(m.Name, "kube-api-access") || strings.Contains(m.Name, "token") {
				continue
			}
			mounts = append(mounts, m)
		}
		hostPod.Spec.Containers[i].VolumeMounts = mounts
	}

	_, err := s.hostClient.CoreV1().Pods(s.hostNS).Get(ctx, hostName, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		_, err = s.hostClient.CoreV1().Pods(s.hostNS).Create(ctx, hostPod, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("creating host pod: %w", err)
		}
		fmt.Printf("  [pods] synced %s/%s -> %s/%s\n", vPod.Namespace, vPod.Name, s.hostNS, hostName)
		return nil
	} else if err != nil {
		return err
	}

	// Pod already exists on the host. Status updates are driven by the
	// host-pod watcher (syncHostPods); nothing to do here.
	return nil
}

// syncHostPods watches synced pods on the host cluster and reflects their
// status (Phase, Conditions, IPs, ContainerStatuses) back into the virtual
// cluster. It also issues a Bind on the virtual pod once the host pod has
// been scheduled, so that kubectl logs/exec/port-forward — which require
// .spec.nodeName — start working.
func (s *Syncer) syncHostPods(ctx context.Context) error {
	selector := fmt.Sprintf("%s=%s", LabelSyncedFrom, s.name)
	return watchWithRetry(ctx, "host-pods", func(ctx context.Context) error {
		watcher, err := s.hostClient.CoreV1().Pods(s.hostNS).Watch(ctx, metav1.ListOptions{
			LabelSelector: selector,
		})
		if err != nil {
			return fmt.Errorf("watching host pods: %w", err)
		}
		defer watcher.Stop()

		for {
			select {
			case <-ctx.Done():
				return nil
			case event, ok := <-watcher.ResultChan():
				if !ok {
					return fmt.Errorf("watch channel closed")
				}
				hostPod, ok := event.Object.(*corev1.Pod)
				if !ok {
					continue
				}
				switch event.Type {
				case watch.Added, watch.Modified:
					if err := s.reconcileVirtualPodFromHost(ctx, hostPod); err != nil {
						fmt.Printf("  [host-pods] reconcile error for %s: %v\n", hostPod.Name, err)
					}
				}
			}
		}
	})
}

// reconcileVirtualPodFromHost copies the host pod's status onto the matching
// virtual pod, and binds the virtual pod to its host node if not yet bound.
func (s *Syncer) reconcileVirtualPodFromHost(ctx context.Context, hostPod *corev1.Pod) error {
	vName := hostPod.Labels[LabelVirtualName]
	vNamespace := hostPod.Labels[LabelVirtualNamespace]
	if vName == "" || vNamespace == "" {
		return nil
	}

	vPod, err := s.vClient.CoreV1().Pods(vNamespace).Get(ctx, vName, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		// Virtual pod is gone (likely being deleted); the virtual-side
		// watcher will handle deletion of the host pod.
		return nil
	}
	if err != nil {
		return fmt.Errorf("getting virtual pod: %w", err)
	}

	// 1. Bind the virtual pod to a node if it isn't yet, so kubectl
	//    logs/exec stop returning "pod does not have a host assigned".
	if vPod.Spec.NodeName == "" && hostPod.Spec.NodeName != "" {
		binding := &corev1.Binding{
			ObjectMeta: metav1.ObjectMeta{
				Name:      vPod.Name,
				Namespace: vPod.Namespace,
			},
			Target: corev1.ObjectReference{
				Kind: "Node",
				Name: hostPod.Spec.NodeName,
			},
		}
		if err := s.vClient.CoreV1().Pods(vPod.Namespace).Bind(ctx, binding, metav1.CreateOptions{}); err != nil && !errors.IsConflict(err) && !errors.IsAlreadyExists(err) {
			return fmt.Errorf("binding virtual pod: %w", err)
		}

		// Re-fetch so the status update below uses the latest resourceVersion.
		vPod, err = s.vClient.CoreV1().Pods(vNamespace).Get(ctx, vName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("re-getting virtual pod after bind: %w", err)
		}
	}

	// 2. Copy host pod status onto the virtual pod when it differs.
	if reflect.DeepEqual(vPod.Status, hostPod.Status) {
		return nil
	}
	vPod.Status = *hostPod.Status.DeepCopy()
	if _, err := s.vClient.CoreV1().Pods(vPod.Namespace).UpdateStatus(ctx, vPod, metav1.UpdateOptions{}); err != nil {
		if errors.IsConflict(err) {
			// Will be retried on the next watch event.
			return nil
		}
		return fmt.Errorf("updating virtual pod status: %w", err)
	}
	return nil
}

// syncServices watches services in the virtual cluster and syncs them to the host.
func (s *Syncer) syncServices(ctx context.Context) error {
	return watchWithRetry(ctx, "services", func(ctx context.Context) error {
		watcher, err := s.vClient.CoreV1().Services("").Watch(ctx, metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("watching services: %w", err)
		}
		defer watcher.Stop()

		for {
			select {
			case <-ctx.Done():
				return nil
			case event, ok := <-watcher.ResultChan():
				if !ok {
					return fmt.Errorf("watch channel closed")
				}
				svc, ok := event.Object.(*corev1.Service)
				if !ok || isSystemNamespace(svc.Namespace) {
					continue
				}
				// Skip the kubernetes default service
				if svc.Name == "kubernetes" && svc.Namespace == "default" {
					continue
				}

				switch event.Type {
				case watch.Added, watch.Modified:
					if err := s.syncServiceToHost(ctx, svc); err != nil {
						fmt.Printf("  [services] sync error for %s/%s: %v\n", svc.Namespace, svc.Name, err)
					}
				case watch.Deleted:
					hostName := s.HostName(svc.Name, svc.Namespace)
					err := s.hostClient.CoreV1().Services(s.hostNS).Delete(ctx, hostName, metav1.DeleteOptions{})
					if err != nil && !errors.IsNotFound(err) {
						fmt.Printf("  [services] delete error for %s: %v\n", hostName, err)
					} else if err == nil {
						fmt.Printf("  [services] deleted %s/%s -> %s/%s\n", svc.Namespace, svc.Name, s.hostNS, hostName)
					}
				}
			}
		}
	})
}

func (s *Syncer) syncServiceToHost(ctx context.Context, vSvc *corev1.Service) error {
	hostName := s.HostName(vSvc.Name, vSvc.Namespace)
	labels := s.syncLabels(vSvc.Name, vSvc.Namespace)

	// Rewrite selector to match synced pod names
	selector := make(map[string]string)
	for k, v := range vSvc.Spec.Selector {
		selector[k] = v
	}
	// Add our label so it only matches synced pods
	selector[LabelSyncedFrom] = s.name

	hostSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        hostName,
			Namespace:   s.hostNS,
			Labels:      labels,
			Annotations: vSvc.Annotations,
		},
		Spec: corev1.ServiceSpec{
			Type:     vSvc.Spec.Type,
			Selector: selector,
			Ports:    vSvc.Spec.Ports,
		},
	}

	// ClusterIP must be empty for create
	hostSvc.Spec.ClusterIP = ""
	hostSvc.Spec.ClusterIPs = nil

	existing, err := s.hostClient.CoreV1().Services(s.hostNS).Get(ctx, hostName, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		_, err = s.hostClient.CoreV1().Services(s.hostNS).Create(ctx, hostSvc, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("creating host service: %w", err)
		}
		fmt.Printf("  [services] synced %s/%s -> %s/%s\n", vSvc.Namespace, vSvc.Name, s.hostNS, hostName)
		return nil
	} else if err != nil {
		return err
	}

	// Update existing
	existing.Spec.Ports = hostSvc.Spec.Ports
	existing.Spec.Selector = hostSvc.Spec.Selector
	existing.Labels = labels
	_, err = s.hostClient.CoreV1().Services(s.hostNS).Update(ctx, existing, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("updating host service: %w", err)
	}
	return nil
}

// syncConfigMaps watches configmaps in the virtual cluster and syncs them to the host.
func (s *Syncer) syncConfigMaps(ctx context.Context) error {
	return watchWithRetry(ctx, "configmaps", func(ctx context.Context) error {
		watcher, err := s.vClient.CoreV1().ConfigMaps("").Watch(ctx, metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("watching configmaps: %w", err)
		}
		defer watcher.Stop()

		for {
			select {
			case <-ctx.Done():
				return nil
			case event, ok := <-watcher.ResultChan():
				if !ok {
					return fmt.Errorf("watch channel closed")
				}
				cm, ok := event.Object.(*corev1.ConfigMap)
				if !ok || isSystemNamespace(cm.Namespace) {
					continue
				}
				// Skip kube-root-ca.crt
				if cm.Name == "kube-root-ca.crt" {
					continue
				}

				switch event.Type {
				case watch.Added, watch.Modified:
					if err := s.syncConfigMapToHost(ctx, cm); err != nil {
						fmt.Printf("  [configmaps] sync error for %s/%s: %v\n", cm.Namespace, cm.Name, err)
					}
				case watch.Deleted:
					hostName := s.HostName(cm.Name, cm.Namespace)
					err := s.hostClient.CoreV1().ConfigMaps(s.hostNS).Delete(ctx, hostName, metav1.DeleteOptions{})
					if err != nil && !errors.IsNotFound(err) {
						fmt.Printf("  [configmaps] delete error for %s: %v\n", hostName, err)
					} else if err == nil {
						fmt.Printf("  [configmaps] deleted %s/%s -> %s/%s\n", cm.Namespace, cm.Name, s.hostNS, hostName)
					}
				}
			}
		}
	})
}

func (s *Syncer) syncConfigMapToHost(ctx context.Context, vCM *corev1.ConfigMap) error {
	hostName := s.HostName(vCM.Name, vCM.Namespace)
	labels := s.syncLabels(vCM.Name, vCM.Namespace)

	hostCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:        hostName,
			Namespace:   s.hostNS,
			Labels:      labels,
			Annotations: vCM.Annotations,
		},
		Data:       vCM.Data,
		BinaryData: vCM.BinaryData,
	}

	existing, err := s.hostClient.CoreV1().ConfigMaps(s.hostNS).Get(ctx, hostName, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		_, err = s.hostClient.CoreV1().ConfigMaps(s.hostNS).Create(ctx, hostCM, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("creating host configmap: %w", err)
		}
		fmt.Printf("  [configmaps] synced %s/%s -> %s/%s\n", vCM.Namespace, vCM.Name, s.hostNS, hostName)
		return nil
	} else if err != nil {
		return err
	}

	existing.Data = hostCM.Data
	existing.BinaryData = hostCM.BinaryData
	existing.Labels = labels
	_, err = s.hostClient.CoreV1().ConfigMaps(s.hostNS).Update(ctx, existing, metav1.UpdateOptions{})
	return err
}

// syncSecrets watches secrets in the virtual cluster and syncs them to the host.
func (s *Syncer) syncSecrets(ctx context.Context) error {
	return watchWithRetry(ctx, "secrets", func(ctx context.Context) error {
		watcher, err := s.vClient.CoreV1().Secrets("").Watch(ctx, metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("watching secrets: %w", err)
		}
		defer watcher.Stop()

		for {
			select {
			case <-ctx.Done():
				return nil
			case event, ok := <-watcher.ResultChan():
				if !ok {
					return fmt.Errorf("watch channel closed")
				}
				secret, ok := event.Object.(*corev1.Secret)
				if !ok || isSystemNamespace(secret.Namespace) {
					continue
				}
				if isSystemSecret(secret) {
					continue
				}

				switch event.Type {
				case watch.Added, watch.Modified:
					if err := s.syncSecretToHost(ctx, secret); err != nil {
						fmt.Printf("  [secrets] sync error for %s/%s: %v\n", secret.Namespace, secret.Name, err)
					}
				case watch.Deleted:
					hostName := s.HostName(secret.Name, secret.Namespace)
					err := s.hostClient.CoreV1().Secrets(s.hostNS).Delete(ctx, hostName, metav1.DeleteOptions{})
					if err != nil && !errors.IsNotFound(err) {
						fmt.Printf("  [secrets] delete error for %s: %v\n", hostName, err)
					} else if err == nil {
						fmt.Printf("  [secrets] deleted %s/%s -> %s/%s\n", secret.Namespace, secret.Name, s.hostNS, hostName)
					}
				}
			}
		}
	})
}

func (s *Syncer) syncSecretToHost(ctx context.Context, vSecret *corev1.Secret) error {
	hostName := s.HostName(vSecret.Name, vSecret.Namespace)
	labels := s.syncLabels(vSecret.Name, vSecret.Namespace)

	hostSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        hostName,
			Namespace:   s.hostNS,
			Labels:      labels,
			Annotations: vSecret.Annotations,
		},
		Data:       vSecret.Data,
		StringData: vSecret.StringData,
		Type:       vSecret.Type,
	}

	existing, err := s.hostClient.CoreV1().Secrets(s.hostNS).Get(ctx, hostName, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		_, err = s.hostClient.CoreV1().Secrets(s.hostNS).Create(ctx, hostSecret, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("creating host secret: %w", err)
		}
		fmt.Printf("  [secrets] synced %s/%s -> %s/%s\n", vSecret.Namespace, vSecret.Name, s.hostNS, hostName)
		return nil
	} else if err != nil {
		return err
	}

	existing.Data = hostSecret.Data
	existing.StringData = hostSecret.StringData
	existing.Labels = labels
	_, err = s.hostClient.CoreV1().Secrets(s.hostNS).Update(ctx, existing, metav1.UpdateOptions{})
	return err
}

// syncNodes syncs nodes from host cluster to virtual cluster (read-only).
func (s *Syncer) syncNodes(ctx context.Context) error {
	return watchWithRetry(ctx, "nodes", func(ctx context.Context) error {
		watcher, err := s.hostClient.CoreV1().Nodes().Watch(ctx, metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("watching nodes: %w", err)
		}
		defer watcher.Stop()

		for {
			select {
			case <-ctx.Done():
				return nil
			case event, ok := <-watcher.ResultChan():
				if !ok {
					return fmt.Errorf("watch channel closed")
				}
				node, ok := event.Object.(*corev1.Node)
				if !ok {
					continue
				}

				switch event.Type {
				case watch.Added, watch.Modified:
					if err := s.syncNodeToVirtual(ctx, node); err != nil {
						fmt.Printf("  [nodes] sync error for %s: %v\n", node.Name, err)
					}
				}
			}
		}
	})
}

func (s *Syncer) syncNodeToVirtual(ctx context.Context, hostNode *corev1.Node) error {
	vNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   hostNode.Name,
			Labels: hostNode.Labels,
		},
		Spec:   hostNode.Spec,
		Status: *hostNode.Status.DeepCopy(),
	}
	s.rewriteNodeForShim(&vNode.Status)

	existing, err := s.vClient.CoreV1().Nodes().Get(ctx, hostNode.Name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		_, err = s.vClient.CoreV1().Nodes().Create(ctx, vNode, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("creating virtual node: %w", err)
		}
		fmt.Printf("  [nodes] synced %s -> virtual\n", hostNode.Name)
		return nil
	} else if err != nil {
		return err
	}

	existing.Status = *hostNode.Status.DeepCopy()
	existing.Labels = hostNode.Labels
	s.rewriteNodeForShim(&existing.Status)
	_, err = s.vClient.CoreV1().Nodes().UpdateStatus(ctx, existing, metav1.UpdateOptions{})
	return err
}

// rewriteNodeForShim mutates a node Status so that kubelet API requests sent
// by the virtual k3s API server land on this syncer's kubelet shim instead of
// the real host kubelet. The k3s API server is invoked with
// --kubelet-preferred-address-types=InternalIP, so we only need to swap
// every InternalIP for the syncer pod IP and update the kubelet port.
//
// We *only* rewrite when the syncer is configured with a shim. In test
// fixtures and in pre-shim setups the node addresses are passed through
// untouched.
func (s *Syncer) rewriteNodeForShim(status *corev1.NodeStatus) {
	if s.ShimPodIP == "" || s.ShimPort == 0 {
		return
	}
	// Replace InternalIPs with the shim address. Keep Hostname / ExternalIP
	// entries untouched so users still see meaningful info in
	// `kubectl get node -o wide`.
	rewrote := false
	for i := range status.Addresses {
		if status.Addresses[i].Type == corev1.NodeInternalIP {
			status.Addresses[i].Address = s.ShimPodIP
			rewrote = true
		}
	}
	if !rewrote {
		status.Addresses = append(status.Addresses, corev1.NodeAddress{
			Type:    corev1.NodeInternalIP,
			Address: s.ShimPodIP,
		})
	}
	status.DaemonEndpoints.KubeletEndpoint.Port = s.ShimPort
}

func boolPtr(b bool) *bool {
	return &b
}
