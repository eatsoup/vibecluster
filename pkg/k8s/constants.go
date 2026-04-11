package k8s

const (
	// LabelManagedBy is the label key for identifying vibecluster-managed resources.
	LabelManagedBy = "app.kubernetes.io/managed-by"
	// LabelManagedByValue is the value for the managed-by label.
	LabelManagedByValue = "vibecluster"
	// LabelVClusterName is the label key for the virtual cluster name.
	LabelVClusterName = "vibecluster.dev/name"
	// AnnotationCreated is the annotation for creation timestamp.
	AnnotationCreated = "vibecluster.dev/created"

	// K3sImage is the default k3s container image.
	K3sImage = "rancher/k3s:v1.28.5-k3s1"
	// SyncerImage is the default syncer container image.
	SyncerImage = "ghcr.io/eatsoup/vibecluster/syncer:latest"
	// K3sPort is the k3s API server port.
	K3sPort = 6443
	// ServicePort is the exposed service port.
	ServicePort = 443
	// KubeletShimPort is the port the syncer's kubelet shim listens on.
	// The synced virtual node's daemonEndpoints.kubeletEndpoint.port is set
	// to this value so the virtual k3s API server forwards logs/exec/portforward
	// requests to the shim instead of the real host kubelet.
	KubeletShimPort = 10250

	// NamespacePrefix is the prefix for vibecluster namespaces.
	NamespacePrefix = "vc-"

	// KubeconfigSecretSuffix is the suffix for kubeconfig secrets.
	KubeconfigSecretSuffix = "-kubeconfig"
)

// NamespaceName returns the namespace name for a virtual cluster.
func NamespaceName(name string) string {
	return NamespacePrefix + name
}

// Labels returns the standard labels for a virtual cluster resource.
func Labels(name string) map[string]string {
	return map[string]string{
		LabelManagedBy:    LabelManagedByValue,
		LabelVClusterName: name,
		"app":             "vibecluster",
	}
}
