package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
	"github.com/craftainer-labs/lib-operator-go/pkg/cloud"
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
//
// The fields have no schema defaults, because a store with a provider forbids
// region, pathStyle and backend (docs/crd-drafts.md, "Cloud providers"). The
// controller applies the same values the schema used to: region us-east-1,
// pathStyle true, backend RustFS (see DefaultRegion, PathStyleOf and
// DefaultBackend), so a store that predates the provider reads as before.
type ObjectStoreConfig struct {
	// endpoint is the https URL of the store; the admin API is under it.
	// Required without a provider; with one it is an optional override of the
	// discovered address.
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:XValidation:rule="self.startsWith('https://')",message="endpoint must be an https URL"
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// region is the S3 region the store reports (default us-east-1). Self-hosted
	// stores only: a provider names its region in provider.aws.region.
	// +kubebuilder:validation:MaxLength=64
	// +optional
	Region string `json:"region,omitempty"`

	// pathStyle selects path-style addressing (https://host/bucket) over
	// virtual-hosted style (default true). It is a pointer so that false
	// survives omitempty. Self-hosted stores only.
	// +optional
	PathStyle *bool `json:"pathStyle,omitempty"`

	// backend is the object store implementation (default RustFS). Self-hosted
	// stores only.
	// +optional
	Backend Backend `json:"backend,omitempty"`
}

// Controller defaults of the fields above.
const (
	// DefaultRegion is the signing region of a store that names none.
	DefaultRegion = "us-east-1"
	// DefaultBackend is the backend of a store that names none.
	DefaultBackend = BackendRustFS
)

// RegionOf returns the region of a self-hosted store, DefaultRegion when unset.
func (c ObjectStoreConfig) RegionOf() string {
	if c.Region == "" {
		return DefaultRegion
	}
	return c.Region
}

// PathStyleOf reports whether the store is addressed path-style: true unless
// pathStyle is set to false.
func (c ObjectStoreConfig) PathStyleOf() bool { return c.PathStyle == nil || *c.PathStyle }

// BackendOf returns the backend of a store, DefaultBackend when unset.
func (c ObjectStoreConfig) BackendOf() Backend {
	if c.Backend == "" {
		return DefaultBackend
	}
	return c.Backend
}

// NamespacedStoreProvider is the cloud a namespaced store runs in: AWS or GCP. Azure follows
// with its own NKs3 item.
//
// +kubebuilder:validation:XValidation:rule="self.type == 'AWS' || self.type == 'GCP'",message="only AWS and GCP are supported as provider.type"
// +kubebuilder:validation:XValidation:rule="(self.type == 'AWS') == has(self.aws)",message="provider.aws is required when type is AWS, and forbidden otherwise"
// +kubebuilder:validation:XValidation:rule="(self.type == 'GCP') == has(self.gcp)",message="provider.gcp is required when type is GCP, and forbidden otherwise"
type NamespacedStoreProvider struct {
	// type selects the cloud; it cannot change. The schema lists every cloud
	// of lib-operator-go, and the rules above admit only AWS and GCP: Azure
	// comes with its own NKs3 item.
	Type cloud.ProviderType `json:"type"`

	// aws is the AWS block, set exactly when type is AWS.
	// +optional
	AWS *NamespacedAWSBlock `json:"aws,omitempty"`

	// gcp is the GCP block, set exactly when type is GCP: the project, and
	// the location buckets are created in (empty is GCS's default, US).
	// +optional
	GCP *cloud.GCPBlock `json:"gcp,omitempty"`
}

// ObjectStoreSpec defines the desired state of ObjectStore.
//
// +kubebuilder:validation:XValidation:rule="has(self.provider) || has(self.endpoint)",message="endpoint is required without a provider"
// +kubebuilder:validation:XValidation:rule="has(self.provider) || has(self.adminCredentialsSecretRef)",message="adminCredentialsSecretRef is required without a provider"
// +kubebuilder:validation:XValidation:rule="!has(self.provider) || !has(self.adminCredentialsSecretRef)",message="adminCredentialsSecretRef is forbidden with a provider: the cloud credential is credentials"
// +kubebuilder:validation:XValidation:rule="!has(self.provider) || (!has(self.region) && !has(self.pathStyle) && !has(self.backend))",message="region, pathStyle and backend are forbidden with a provider"
// +kubebuilder:validation:XValidation:rule="!has(self.credentials) || has(self.provider)",message="credentials is allowed only with a provider"
// +kubebuilder:validation:XValidation:rule="!has(self.provider) || has(self.credentials)",message="a namespaced store with a provider needs credentials with source Secret"
// +kubebuilder:validation:XValidation:rule="!has(self.adminCredentialsSecretRef) || !has(self.adminCredentialsSecretRef.namespace)",message="adminCredentialsSecretRef.namespace must not be set: the Secret is in the ObjectStore's namespace"
// +kubebuilder:validation:XValidation:rule="has(self.provider) == has(oldSelf.provider)",message="provider cannot be added or removed: a store cannot move between clouds or between cloud and self-hosted"
type ObjectStoreSpec struct {
	common.CloudServerCommon `json:",inline"`
	ObjectStoreConfig        `json:",inline"`

	// provider runs the store on a cloud (S3 buckets of an AWS account).
	// Absent means a self-hosted RustFS.
	// +optional
	Provider *NamespacedStoreProvider `json:"provider,omitempty"`

	// credentials is the cloud credential. A namespaced store may not use the
	// operator's own identity, so it must be a Secret in the store's namespace.
	// +optional
	Credentials *cloud.NamespacedCredentials `json:"credentials,omitempty"`
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

// GetCloudCommon returns the settings every cloud-capable server shares.
func (s *ObjectStore) GetCloudCommon() *common.CloudServerCommon { return &s.Spec.CloudServerCommon }

// GetServerStatus returns the status the probe writes (probe.CloudServer).
func (s *ObjectStore) GetServerStatus() *common.ServerStatus { return &s.Status }

// GetEndpoint returns spec.endpoint, empty for a store whose address is
// discovered.
func (s *ObjectStore) GetEndpoint() string { return s.Spec.Endpoint }

// GetAllowedNamespaces returns nil: a namespaced store serves only its own
// namespace.
func (s *ObjectStore) GetAllowedNamespaces() *common.AllowedNamespaces { return nil }

// GetCredentialsSecret returns the name of spec.credentials.secretRef, and an
// empty name when the store has none. The namespace is empty: the Secret is in
// the store's namespace.
func (s *ObjectStore) GetCredentialsSecret() (name, namespace string) {
	c := s.Spec.Credentials
	if c == nil || c.Source != cloud.SourceSecret || c.SecretRef == nil {
		return "", ""
	}
	return c.SecretRef.Name, c.SecretRef.Namespace
}

// HasProvider reports whether the store runs on a cloud.
func (s *ObjectStore) HasProvider() bool { return s.Spec.Provider != nil }

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ObjectStore{}, &ObjectStoreList{})
		return nil
	})
}
