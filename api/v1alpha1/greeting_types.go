package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// DeletionPolicy says what happens to the object a Greeting provisioned when
// the Greeting is deleted.
// +kubebuilder:validation:Enum=Delete;Retain
type DeletionPolicy string

const (
	// DeletionPolicyDelete removes the provisioned object with the Greeting.
	DeletionPolicyDelete DeletionPolicy = "Delete"
	// DeletionPolicyRetain leaves the provisioned object in place.
	DeletionPolicyRetain DeletionPolicy = "Retain"
)

// GreetingSpec defines the desired state of Greeting.
//
// The sample API stands in for an operator's real kinds: it provisions one
// "external" object (a ConfigMap, so the template needs no backend service)
// and follows the conventions every operator of the family uses: schema
// defaults, a deletion policy, a finalizer, and Ready/Synced conditions.
type GreetingSpec struct {
	// message is the text written to the provisioned ConfigMap's `message` key.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:default="Hello"
	// +optional
	Message string `json:"message,omitempty"`

	// deletionPolicy decides whether the ConfigMap is deleted with the
	// Greeting (Delete) or left behind (Retain).
	// +kubebuilder:default=Delete
	// +optional
	DeletionPolicy DeletionPolicy `json:"deletionPolicy,omitempty"`
}

// GreetingStatus defines the observed state of Greeting.
type GreetingStatus struct {
	// observedGeneration is the metadata.generation the operator last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// configMapName is the name of the provisioned ConfigMap.
	// +optional
	ConfigMapName string `json:"configMapName,omitempty"`

	// conditions represent the current state of the Greeting resource.
	//
	// The operator sets Ready and Synced on the first reconcile, each with
	// observedGeneration, so `kubectl wait --for=condition=Ready` works and a
	// missing condition always means the operator is not running.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=craftainer
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Synced",type=string,JSONPath=`.status.conditions[?(@.type=="Synced")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Greeting is the Schema for the greetings API.
type Greeting struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Greeting
	// +required
	Spec GreetingSpec `json:"spec"`

	// status defines the observed state of Greeting
	// +optional
	Status GreetingStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// GreetingList contains a list of Greeting.
type GreetingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Greeting `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Greeting{}, &GreetingList{})
		return nil
	})
}
