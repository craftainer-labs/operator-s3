package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
)

// +kubebuilder:validation:XValidation:rule="has(self.adminCredentialsSecretRef.namespace) && self.adminCredentialsSecretRef.namespace != ''",message="adminCredentialsSecretRef.namespace is required on a cluster-scoped store"

// ClusterObjectStoreSpec defines the desired state of ClusterObjectStore.
type ClusterObjectStoreSpec struct {
	common.ServerCommon `json:",inline"`
	ObjectStoreConfig   `json:",inline"`

	// allowedNamespaces says which namespaces may claim from this store. It is
	// required, so a store without it is rejected: it fails closed.
	AllowedNamespaces common.AllowedNamespaces `json:"allowedNamespaces"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,categories=craftainer
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Synced",type=string,JSONPath=`.status.conditions[?(@.type=="Synced")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ClusterObjectStore is a cluster-scoped object store that the namespaces in
// allowedNamespaces may claim from.
type ClusterObjectStore struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ClusterObjectStore
	// +required
	Spec ClusterObjectStoreSpec `json:"spec"`

	// status defines the observed state of ClusterObjectStore
	// +optional
	Status common.ServerStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ClusterObjectStoreList contains a list of ClusterObjectStore.
type ClusterObjectStoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ClusterObjectStore `json:"items"`
}

// GetCommon returns the settings every server kind shares.
func (s *ClusterObjectStore) GetCommon() *common.ServerCommon { return &s.Spec.ServerCommon }

// GetServerStatus returns the status the probe writes (probe.Server).
func (s *ClusterObjectStore) GetServerStatus() *common.ServerStatus { return &s.Status }

// GetEndpoint returns spec.endpoint.
func (s *ClusterObjectStore) GetEndpoint() string { return s.Spec.Endpoint }

// GetAllowedNamespaces returns spec.allowedNamespaces.
func (s *ClusterObjectStore) GetAllowedNamespaces() *common.AllowedNamespaces {
	return &s.Spec.AllowedNamespaces
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ClusterObjectStore{}, &ClusterObjectStoreList{})
		return nil
	})
}
