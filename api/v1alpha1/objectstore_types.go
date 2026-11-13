package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
)

// Backend is the object store implementation behind a store.
//
// +kubebuilder:validation:Enum=RustFS
type Backend string

// Values of Backend.
const (
	// BackendRustFS is RustFS, driven through its admin API at
	// <endpoint>/rustfs/admin/v3. MinIO is not supported.
	BackendRustFS Backend = "RustFS"
)

// ObjectStoreConfig is what the two store kinds share besides the server
// blocks of lib-operator-go. It has no type-level rules: each kind's spec
// carries the rules that differ between the cluster and namespaced kind.
type ObjectStoreConfig struct {
	// endpoint is the https URL of the store; the admin API is under it.
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:XValidation:rule="self.startsWith('https://')",message="endpoint must be an https URL"
	Endpoint string `json:"endpoint"`

	// region is the S3 region the store reports.
	// +kubebuilder:default="us-east-1"
	// +kubebuilder:validation:MaxLength=64
	// +optional
	Region string `json:"region,omitempty"`

	// pathStyle selects path-style addressing (https://host/bucket) over
	// virtual-hosted style. It is a pointer so that false survives omitempty.
	// +kubebuilder:default=true
	// +optional
	PathStyle *bool `json:"pathStyle,omitempty"`

	// backend is the object store implementation.
	// +kubebuilder:default=RustFS
	// +optional
	Backend Backend `json:"backend,omitempty"`
}

// ObjectStoreSpec defines the desired state of ObjectStore.
//
// +kubebuilder:validation:XValidation:rule="!has(self.adminCredentialsSecretRef.namespace)",message="adminCredentialsSecretRef.namespace must not be set: the Secret is in the ObjectStore's namespace"
type ObjectStoreSpec struct {
	common.ServerCommon `json:",inline"`
	ObjectStoreConfig   `json:",inline"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=craftainer
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Synced",type=string,JSONPath=`.status.conditions[?(@.type=="Synced")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ObjectStore is an object store that only claims in its own namespace may use.
type ObjectStore struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ObjectStore
	// +required
	Spec ObjectStoreSpec `json:"spec"`

	// status defines the observed state of ObjectStore
	// +optional
	Status common.ServerStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ObjectStoreList contains a list of ObjectStore.
type ObjectStoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ObjectStore `json:"items"`
}

// GetCommon returns the settings every server kind shares.
func (s *ObjectStore) GetCommon() *common.ServerCommon { return &s.Spec.ServerCommon }

// GetServerStatus returns the status the probe writes (probe.Server).
func (s *ObjectStore) GetServerStatus() *common.ServerStatus { return &s.Status }

// GetEndpoint returns spec.endpoint.
func (s *ObjectStore) GetEndpoint() string { return s.Spec.Endpoint }

// GetAllowedNamespaces returns nil: a namespaced store serves only its own
// namespace.
func (s *ObjectStore) GetAllowedNamespaces() *common.AllowedNamespaces { return nil }

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ObjectStore{}, &ObjectStoreList{})
		return nil
	})
}
