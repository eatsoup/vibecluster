package k8s

import (
	"context"
	"fmt"

	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

// VirtualClusterGVR is the GroupVersionResource for the VirtualCluster CRD.
var VirtualClusterGVR = schema.GroupVersionResource{
	Group:    "vibecluster.dev",
	Version:  "v1alpha1",
	Resource: "virtualclusters",
}

// DefaultCROperatorNamespace is where the CLI creates VirtualCluster CRs by default
// when running in operator mode.
const DefaultCROperatorNamespace = "default"

// IsOperatorAvailable returns true if the VirtualCluster CRD is registered in the host cluster.
// A missing CRD (NotFound) returns (false, nil); transient errors are returned as-is.
func IsOperatorAvailable(ctx context.Context, restConfig *rest.Config) (bool, error) {
	extClient, err := apiextensionsclient.NewForConfig(restConfig)
	if err != nil {
		return false, fmt.Errorf("creating apiextensions client: %w", err)
	}
	return isOperatorAvailableWith(ctx, extClient)
}

// isOperatorAvailableWith is the client-injectable variant used by tests.
func isOperatorAvailableWith(ctx context.Context, extClient apiextensionsclient.Interface) (bool, error) {
	_, err := extClient.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, CRDName, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// VirtualClusterCRSpec mirrors the fields the CLI sets when creating a VirtualCluster CR.
// Fields left empty are omitted so the CRD-side defaults apply.
type VirtualClusterCRSpec struct {
	K3sImage    string
	SyncerImage string
	Storage     string
}

// CreateVirtualClusterCR creates a VirtualCluster custom resource via the dynamic client.
func CreateVirtualClusterCR(ctx context.Context, restConfig *rest.Config, name, namespace string, spec VirtualClusterCRSpec) error {
	dynClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("creating dynamic client: %w", err)
	}

	specMap := map[string]interface{}{}
	if spec.K3sImage != "" {
		specMap["k3sImage"] = spec.K3sImage
	}
	if spec.SyncerImage != "" {
		specMap["syncerImage"] = spec.SyncerImage
	}
	if spec.Storage != "" {
		specMap["storage"] = spec.Storage
	}

	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "vibecluster.dev/v1alpha1",
			"kind":       "VirtualCluster",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": namespace,
			},
			"spec": specMap,
		},
	}

	_, err = dynClient.Resource(VirtualClusterGVR).Namespace(namespace).Create(ctx, obj, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		return fmt.Errorf("VirtualCluster %s/%s already exists", namespace, name)
	}
	return err
}

// DeleteVirtualClusterCR deletes a VirtualCluster CR. Returns (true, nil) if a CR was found and
// a delete request was issued, (false, nil) if no CR exists, or (false, err) on error.
func DeleteVirtualClusterCR(ctx context.Context, restConfig *rest.Config, name, namespace string) (bool, error) {
	dynClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return false, fmt.Errorf("creating dynamic client: %w", err)
	}
	err = dynClient.Resource(VirtualClusterGVR).Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if errors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// FindVirtualClusterCR locates a VirtualCluster CR by name across all namespaces.
// Returns the namespace it lives in, or "" if no CR was found.
func FindVirtualClusterCR(ctx context.Context, restConfig *rest.Config, name string) (string, error) {
	dynClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return "", fmt.Errorf("creating dynamic client: %w", err)
	}
	return findVirtualClusterCRWith(ctx, dynClient, name)
}

// findVirtualClusterCRWith is the client-injectable variant used by tests.
func findVirtualClusterCRWith(ctx context.Context, dynClient dynamic.Interface, name string) (string, error) {
	list, err := dynClient.Resource(VirtualClusterGVR).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return "", nil
		}
		return "", err
	}
	for _, item := range list.Items {
		if item.GetName() == name {
			return item.GetNamespace(), nil
		}
	}
	return "", nil
}

// ListVirtualClusterCRs returns the names+namespaces of all VirtualCluster CRs in the host cluster.
type VClusterCR struct {
	Name      string
	Namespace string
	Phase     string
}

// ListVirtualClusterCRs returns all VirtualCluster CRs across all namespaces.
func ListVirtualClusterCRs(ctx context.Context, restConfig *rest.Config) ([]VClusterCR, error) {
	dynClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("creating dynamic client: %w", err)
	}
	return listVirtualClusterCRsWith(ctx, dynClient)
}

// listVirtualClusterCRsWith is the client-injectable variant used by tests.
func listVirtualClusterCRsWith(ctx context.Context, dynClient dynamic.Interface) ([]VClusterCR, error) {
	list, err := dynClient.Resource(VirtualClusterGVR).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]VClusterCR, 0, len(list.Items))
	for _, item := range list.Items {
		phase, _, _ := unstructured.NestedString(item.Object, "status", "phase")
		out = append(out, VClusterCR{
			Name:      item.GetName(),
			Namespace: item.GetNamespace(),
			Phase:     phase,
		})
	}
	return out, nil
}
