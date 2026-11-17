package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
	"github.com/craftainer-labs/lib-operator-go/pkg/cloud"
)

// ClusterStoreProvider is the cloud a cluster store runs in. Only AWS is
// supported by this operator so far.
//
// +kubebuilder:validation:XValidation:rule="self.type == 'AWS'",message="only AWS is supported as provider.type"
// +kubebuilder:validation:XValidation:rule="has(self.aws)",message="provider.aws is required"
type ClusterStoreProvider struct {
	// type selects the cloud; it cannot change. The schema lists every cloud
	// of lib-operator-go, and the rules above admit only AWS: the other clouds
	// come with their own NKs3 items.
	Type cloud.ProviderType `json:"type"`

	// aws is the AWS block, set exactly when type is AWS.
	// +optional
	AWS *cloud.AWSClusterBlock `json:"aws,omitempty"`
}

// ClusterObjectStoreSpec defines the desired state of ClusterObjectStore.
//
// +kubebuilder:validation:XValidation:rule="has(self.provider) || has(self.endpoint)",message="endpoint is required without a provider"
// +kubebuilder:validation:XValidation:rule="has(self.provider) || has(self.adminCredentialsSecretRef)",message="adminCredentialsSecretRef is required without a provider"
// +kubebuilder:validation:XValidation:rule="!has(self.provider) || !has(self.adminCredentialsSecretRef)",message="adminCredentialsSecretRef is forbidden with a provider: the cloud credential is credentials"
// +kubebuilder:validation:XValidation:rule="!has(self.provider) || (!has(self.region) && !has(self.pathStyle) && !has(self.backend))",message="region, pathStyle and backend are forbidden with a provider"
// +kubebuilder:validation:XValidation:rule="!has(self.credentials) || has(self.provider)",message="credentials is allowed only with a provider"
// +kubebuilder:validation:XValidation:rule="!has(self.adminCredentialsSecretRef) || (has(self.adminCredentialsSecretRef.namespace) && self.adminCredentialsSecretRef.namespace != '')",message="adminCredentialsSecretRef.namespace is required on a cluster-scoped store"
// +kubebuilder:validation:XValidation:rule="has(self.provider) == has(oldSelf.provider)",message="provider cannot be added or removed: a store cannot move between clouds or between cloud and self-hosted"
type ClusterObjectStoreSpec struct {
	common.CloudServerCommon `json:",inline"`
	ObjectStoreConfig        `json:",inline"`

	// provider runs the store on a cloud (S3 buckets of an AWS account).
	// Absent means a self-hosted RustFS.
	// +optional
	Provider *ClusterStoreProvider `json:"provider,omitempty"`

	// credentials is the cloud credential: the operator's own identity
	// (WorkloadIdentity, the default) or a Secret in the operator's namespace.
	// +optional
	Credentials *cloud.Credentials `json:"credentials,omitempty"`

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

// GetCloudCommon returns the settings every cloud-capable server shares.
func (s *ClusterObjectStore) GetCloudCommon() *common.CloudServerCommon {
	return &s.Spec.CloudServerCommon
}

// GetServerStatus returns the status the probe writes (probe.CloudServer).
func (s *ClusterObjectStore) GetServerStatus() *common.ServerStatus { return &s.Status }

// GetEndpoint returns spec.endpoint, empty for a store whose address is
// discovered.
func (s *ClusterObjectStore) GetEndpoint() string { return s.Spec.Endpoint }

// GetCredentialsSecret returns the name and namespace of
// spec.credentials.secretRef, and an empty name for workload identity.
func (s *ClusterObjectStore) GetCredentialsSecret() (name, namespace string) {
	c := s.Spec.Credentials
	if c == nil || c.Source != cloud.SourceSecret || c.SecretRef == nil {
		return "", ""
	}
	return c.SecretRef.Name, c.SecretRef.Namespace
}

// HasProvider reports whether the store runs on a cloud.
func (s *ClusterObjectStore) HasProvider() bool { return s.Spec.Provider != nil }

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
