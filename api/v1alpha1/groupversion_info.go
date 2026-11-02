// Package v1alpha1 contains the API schema definitions of the
// s3.craftainer.dev group, version v1alpha1: ClusterObjectStore, ObjectStore,
// Bucket and BucketAccess. The shared blocks (server, claim and status
// fields) come from lib-operator-go's pkg/apis/common; docs/ and the
// lib-operator-go ADR 0002 say what each field means.
// +kubebuilder:object:generate=true
// +groupName=s3.craftainer.dev
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// SchemeGroupVersion is group version used to register these objects.
	// This name is used by applyconfiguration generators (e.g. controller-gen).
	SchemeGroupVersion = schema.GroupVersion{Group: "s3.craftainer.dev", Version: "v1alpha1"}

	// GroupVersion is an alias for SchemeGroupVersion, for backward compatibility.
	GroupVersion = SchemeGroupVersion

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(func(scheme *runtime.Scheme) error {
		metav1.AddToGroupVersion(scheme, SchemeGroupVersion)
		return nil
	})

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
