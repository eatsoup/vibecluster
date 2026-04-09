// Package v1alpha1 contains API Schema definitions for the vibecluster v1alpha1 API group.
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// VirtualClusterSpec defines the desired state of a VirtualCluster.
type VirtualClusterSpec struct {
	// K3sImage is the k3s container image to use.
	// +optional
	// +kubebuilder:default="rancher/k3s:v1.28.5-k3s1"
	K3sImage string `json:"k3sImage,omitempty"`

	// SyncerImage is the syncer container image to use.
	// +optional
	// +kubebuilder:default="ghcr.io/eatsoup/vibecluster/syncer:latest"
	SyncerImage string `json:"syncerImage,omitempty"`

	// Storage is the size of the persistent volume for k3s data.
	// +optional
	// +kubebuilder:default="5Gi"
	Storage string `json:"storage,omitempty"`
}

// VirtualClusterPhase represents the lifecycle phase of a VirtualCluster.
// +kubebuilder:validation:Enum=Pending;Running;Failed;Deleting
type VirtualClusterPhase string

const (
	// VirtualClusterPhasePending means the virtual cluster is being created.
	VirtualClusterPhasePending VirtualClusterPhase = "Pending"
	// VirtualClusterPhaseRunning means the virtual cluster is up and ready.
	VirtualClusterPhaseRunning VirtualClusterPhase = "Running"
	// VirtualClusterPhaseFailed means the virtual cluster creation failed.
	VirtualClusterPhaseFailed VirtualClusterPhase = "Failed"
	// VirtualClusterPhaseDeleting means the virtual cluster is being torn down.
	VirtualClusterPhaseDeleting VirtualClusterPhase = "Deleting"
)

// VirtualClusterStatus defines the observed state of a VirtualCluster.
type VirtualClusterStatus struct {
	// Phase is the current lifecycle phase of the virtual cluster.
	// +optional
	Phase VirtualClusterPhase `json:"phase,omitempty"`

	// Ready indicates whether the virtual cluster is fully operational.
	// +optional
	Ready bool `json:"ready,omitempty"`

	// Message provides human-readable information about the current state.
	// +optional
	Message string `json:"message,omitempty"`

	// Namespace is the host namespace where virtual cluster resources are deployed.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// ObservedGeneration is the most recent generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=vc
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=`.status.ready`
// +kubebuilder:printcolumn:name="Namespace",type=string,JSONPath=`.status.namespace`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// VirtualCluster is the Schema for the virtualclusters API.
// It represents a lightweight virtual Kubernetes cluster running inside the host cluster.
type VirtualCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VirtualClusterSpec   `json:"spec,omitempty"`
	Status VirtualClusterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// VirtualClusterList contains a list of VirtualCluster resources.
type VirtualClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VirtualCluster `json:"items"`
}
