package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
)

// AccessLevel is what a BucketAccess may do in its bucket.
//
// +kubebuilder:validation:Enum=ReadOnly;ReadWrite
type AccessLevel string

// Values of AccessLevel.
const (
	// AccessReadOnly allows listing and reading objects.
	AccessReadOnly AccessLevel = "ReadOnly"
	// AccessReadWrite allows listing, reading, writing and deleting objects.
	AccessReadWrite AccessLevel = "ReadWrite"
)

// BucketRef names a Bucket in the BucketAccess's own namespace.
//
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="bucketRef is immutable"
type BucketRef struct {
	// name of the Bucket.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// BucketAccessSpec defines the desired state of BucketAccess.
//
// +kubebuilder:validation:XValidation:rule="self.serverRef.kind in ['ClusterObjectStore', 'ObjectStore']",message="serverRef.kind must be ClusterObjectStore or ObjectStore"
// +kubebuilder:validation:XValidation:rule="!has(self.rotationPolicy) || self.managementPolicies == ['*'] || 'Update' in self.managementPolicies",message="rotationPolicy needs the Update management policy"
type BucketAccessSpec struct {
	common.ClaimCommon `json:",inline"`
	common.Credentials `json:",inline"`

	// bucketRef is the Bucket the access key is for. It is immutable.
	BucketRef BucketRef `json:"bucketRef"`

	// access is what the key may do.
	// +kubebuilder:default=ReadWrite
	// +optional
	Access AccessLevel `json:"access,omitempty"`

	// prefix restricts the key to the object keys under it, without a leading
	// slash. Empty means the whole bucket.
	// +kubebuilder:validation:MaxLength=1024
	// +kubebuilder:validation:XValidation:rule="!self.startsWith('/')",message="prefix must not start with /"
	// +kubebuilder:validation:XValidation:rule="!self.matches('[*?$]')",message="prefix must not contain *, ? or $"
	// +optional
	Prefix string `json:"prefix,omitempty"`
}

// BucketAccessStatus defines the observed state of BucketAccess.
type BucketAccessStatus struct {
	common.ClaimStatus       `json:",inline"`
	common.CredentialsStatus `json:",inline"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=craftainer
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Synced",type=string,JSONPath=`.status.conditions[?(@.type=="Synced")].status`
// +kubebuilder:printcolumn:name="Server",type=string,JSONPath=`.spec.serverRef.name`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// BucketAccess is an access key on a Bucket, written to a binding Secret.
type BucketAccess struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of BucketAccess
	// +required
	Spec BucketAccessSpec `json:"spec"`

	// status defines the observed state of BucketAccess
	// +optional
	Status BucketAccessStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// BucketAccessList contains a list of BucketAccess.
type BucketAccessList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []BucketAccess `json:"items"`
}

// The methods below implement lib-operator-go's lifecycle.Claim.

// GetServerRef returns spec.serverRef.
func (a *BucketAccess) GetServerRef() common.ServerRef { return a.Spec.ServerRef }

// GetDeletionPolicy returns "": an access key is always deleted with its claim.
func (a *BucketAccess) GetDeletionPolicy() common.DeletionPolicy { return "" }

// GetManagementPolicies returns spec.managementPolicies.
func (a *BucketAccess) GetManagementPolicies() []common.ManagementPolicy {
	return a.Spec.ManagementPolicies
}

// GetConditions returns status.conditions.
func (a *BucketAccess) GetConditions() *[]metav1.Condition { return &a.Status.Conditions }

// GetBindingSecretName returns spec.binding.secretName, or the claim's name.
func (a *BucketAccess) GetBindingSecretName() string {
	if a.Spec.Binding.SecretName != "" {
		return a.Spec.Binding.SecretName
	}
	return a.Name
}

// SetBindingName sets status.binding.name.
func (a *BucketAccess) SetBindingName(v string) { a.Status.Binding = &common.BindingStatus{Name: v} }

// GetExternalName returns status.externalName.
func (a *BucketAccess) GetExternalName() string { return a.Status.ExternalName }

// SetExternalName sets status.externalName.
func (a *BucketAccess) SetExternalName(v string) { a.Status.ExternalName = v }

// GetExternalID returns status.externalId.
func (a *BucketAccess) GetExternalID() string { return a.Status.ExternalID }

// SetExternalID sets status.externalId.
func (a *BucketAccess) SetExternalID(v string) { a.Status.ExternalID = v }

// SetObservedGeneration sets status.observedGeneration.
func (a *BucketAccess) SetObservedGeneration(v int64) { a.Status.ObservedGeneration = v }

// SetLastReconcileTime sets status.lastReconcileTime.
func (a *BucketAccess) SetLastReconcileTime(v metav1.Time) { a.Status.LastReconcileTime = &v }

// GetEffectiveDeletionPolicy returns "": an access key has no pinned policy.
func (a *BucketAccess) GetEffectiveDeletionPolicy() common.DeletionPolicy { return "" }

// SetEffectiveDeletionPolicy does nothing: an access key has no pinned policy.
func (a *BucketAccess) SetEffectiveDeletionPolicy(common.DeletionPolicy, common.DeletionPolicySource) {
}

// SetLateInitialized sets status.lateInitialized.
func (a *BucketAccess) SetLateInitialized(v map[string]string) { a.Status.LateInitialized = v }

// GetRotationPolicy returns spec.rotationPolicy.
func (a *BucketAccess) GetRotationPolicy() *common.RotationPolicy { return a.Spec.RotationPolicy }

// GetRotationStatus returns status.rotation, or the zero status.
func (a *BucketAccess) GetRotationStatus() common.RotationStatus {
	if a.Status.Rotation == nil {
		return common.RotationStatus{}
	}
	return *a.Status.Rotation
}

// SetRotationStatus sets status.rotation.
func (a *BucketAccess) SetRotationStatus(v common.RotationStatus) { a.Status.Rotation = &v }

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &BucketAccess{}, &BucketAccessList{})
		return nil
	})
}
