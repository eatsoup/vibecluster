package syncer

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/eatsoup/vibecluster/pkg/k8s"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
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
	hostClient *kubernetes.Clientset
	vClient    *kubernetes.Clientset
	hostNS     string
}

// New creates a new Syncer.
func New(name string, hostClient, vClient *kubernetes.Clientset) *Syncer {
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
	errCh := make(chan error, 4)

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

	existing, err := s.hostClient.CoreV1().Pods(s.hostNS).Get(ctx, hostName, metav1.GetOptions{})
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

	// Pod exists - sync status back to virtual cluster
	return s.syncPodStatusToVirtual(ctx, existing, vPod)
}

func (s *Syncer) syncPodStatusToVirtual(ctx context.Context, hostPod *corev1.Pod, vPod *corev1.Pod) error {
	if hostPod.Status.Phase == vPod.Status.Phase {
		return nil
	}

	vPod = vPod.DeepCopy()
	vPod.Status = hostPod.Status

	_, err := s.vClient.CoreV1().Pods(vPod.Namespace).UpdateStatus(ctx, vPod, metav1.UpdateOptions{})
	if err != nil && !errors.IsConflict(err) {
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
		Status: hostNode.Status,
	}

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

	existing.Status = hostNode.Status
	existing.Labels = hostNode.Labels
	_, err = s.vClient.CoreV1().Nodes().UpdateStatus(ctx, existing, metav1.UpdateOptions{})
	return err
}

func boolPtr(b bool) *bool {
	return &b
}
