package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
)

// BucketQuota limits the size of a bucket.
type BucketQuota struct {
	// hardBytes is the largest size the bucket may reach, for example 50Gi.
	// The admission webhook requires at least 1 byte.
	HardBytes resource.Quantity `json:"hardBytes"`
}

// LifecycleRule expires the objects under a prefix.
type LifecycleRule struct {
	// id names the rule; it is unique within the bucket.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	ID string `json:"id"`

	// prefix limits the rule to the keys under it, without a leading slash.
	// Empty means every key.
	// +kubebuilder:validation:MaxLength=1024
	// +kubebuilder:validation:XValidation:rule="!self.startsWith('/')",message="prefix must not start with /"
	// +optional
	Prefix string `json:"prefix,omitempty"`

	// expirationDays deletes an object this many days after it was created.
	// +kubebuilder:validation:Minimum=1
	ExpirationDays int32 `json:"expirationDays"`
}

// BucketSpec defines the desired state of Bucket.
//
// +kubebuilder:validation:XValidation:rule="self.serverRef.kind in ['ClusterObjectStore', 'ObjectStore']",message="serverRef.kind must be ClusterObjectStore or ObjectStore"
type BucketSpec struct {
	common.ClaimCommon `json:",inline"`
	common.DataClaim   `json:",inline"`

	// versioning keeps every version of an object.
	// +kubebuilder:default=false
	// +optional
	Versioning bool `json:"versioning,omitempty"`

	// objectLock turns on S3 object lock. It can only be set when the bucket
	// is created.
	// +kubebuilder:default=false
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="objectLock is immutable"
	// +optional
	ObjectLock bool `json:"objectLock,omitempty"`

	// quota limits the bucket's size.
	// +optional
	Quota *BucketQuota `json:"quota,omitempty"`

	// lifecycleRules expire objects; the ids are unique.
	// +listType=map
	// +listMapKey=id
	// +kubebuilder:validation:MaxItems=10
	// +optional
	LifecycleRules []LifecycleRule `json:"lifecycleRules,omitempty"`
}

// BucketStatus defines the observed state of Bucket.
type BucketStatus struct {
	common.ClaimStatus     `json:",inline"`
	common.DataClaimStatus `json:",inline"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=craftainer
// +kubebuilder:validation:XValidation:rule="self.metadata.name.matches('^[a-z0-9]([a-z0-9-]*[a-z0-9])?$')",message="metadata.name must not contain dots: it is mapped to a bucket name"
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Synced",type=string,JSONPath=`.status.conditions[?(@.type=="Synced")].status`
// +kubebuilder:printcolumn:name="Server",type=string,JSONPath=`.spec.serverRef.name`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Bucket is a bucket on an object store.
type Bucket struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Bucket
	// +required
	Spec BucketSpec `json:"spec"`

	// status defines the observed state of Bucket
	// +optional
	Status BucketStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// BucketList contains a list of Bucket.
type BucketList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Bucket `json:"items"`
}

// The methods below implement lib-operator-go's lifecycle.Claim.

// GetServerRef returns spec.serverRef.
func (b *Bucket) GetServerRef() common.ServerRef { return b.Spec.ServerRef }

// GetDeletionPolicy returns spec.deletionPolicy, empty when unset.
func (b *Bucket) GetDeletionPolicy() common.DeletionPolicy { return b.Spec.DeletionPolicy }

// GetManagementPolicies returns spec.managementPolicies.
func (b *Bucket) GetManagementPolicies() []common.ManagementPolicy { return b.Spec.ManagementPolicies }

// GetConditions returns status.conditions.
func (b *Bucket) GetConditions() *[]metav1.Condition { return &b.Status.Conditions }

// GetBindingSecretName returns "": a bucket has no binding Secret.
func (b *Bucket) GetBindingSecretName() string { return "" }

// SetBindingName does nothing: a bucket has no binding Secret.
func (b *Bucket) SetBindingName(string) {}

// GetExternalName returns status.externalName.
func (b *Bucket) GetExternalName() string { return b.Status.ExternalName }

// SetExternalName sets status.externalName.
func (b *Bucket) SetExternalName(v string) { b.Status.ExternalName = v }

// GetExternalID returns status.externalId.
func (b *Bucket) GetExternalID() string { return b.Status.ExternalID }

// SetExternalID sets status.externalId.
func (b *Bucket) SetExternalID(v string) { b.Status.ExternalID = v }

// SetObservedGeneration sets status.observedGeneration.
func (b *Bucket) SetObservedGeneration(v int64) { b.Status.ObservedGeneration = v }

// SetLastReconcileTime sets status.lastReconcileTime.
func (b *Bucket) SetLastReconcileTime(v metav1.Time) { b.Status.LastReconcileTime = &v }

// GetEffectiveDeletionPolicy returns status.effectiveDeletionPolicy.
func (b *Bucket) GetEffectiveDeletionPolicy() common.DeletionPolicy {
	return b.Status.EffectiveDeletionPolicy
}

// SetEffectiveDeletionPolicy sets the pinned deletion policy and its source.
func (b *Bucket) SetEffectiveDeletionPolicy(p common.DeletionPolicy, s common.DeletionPolicySource) {
	b.Status.EffectiveDeletionPolicy, b.Status.DeletionPolicySource = p, s
}

// SetLateInitialized sets status.lateInitialized.
func (b *Bucket) SetLateInitialized(v map[string]string) { b.Status.LateInitialized = v }

// GetRotationPolicy returns nil: a bucket has no credentials to rotate.
func (b *Bucket) GetRotationPolicy() *common.RotationPolicy { return nil }

// GetRotationStatus returns the zero status: a bucket does not rotate.
func (b *Bucket) GetRotationStatus() common.RotationStatus { return common.RotationStatus{} }

// SetRotationStatus does nothing: a bucket does not rotate.
func (b *Bucket) SetRotationStatus(common.RotationStatus) {}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Bucket{}, &BucketList{})
		return nil
	})
}
