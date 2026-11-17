package v1alpha1

import "github.com/craftainer-labs/lib-operator-go/pkg/cloud"

// DefaultIAMPath is the path of the IAM users a BucketAccess creates when the
// store names none. The controller applies it; the schema has no default.
const DefaultIAMPath = "/craftainer/"

// IAMTarget holds the fields of provider.aws that say how the IAM users of a
// BucketAccess are made (ADR 0002, section 16). They apply to a user when the
// operator creates it.
type IAMTarget struct {
	// iamPath is the IAM path of the users the operator creates for
	// BucketAccess claims, beginning and ending with a slash. Default
	// /craftainer/.
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:Pattern=`^/([!-~]+/)?$`
	// +optional
	IAMPath string `json:"iamPath,omitempty"`

	// permissionsBoundaryARN is the ARN of a managed policy set as the
	// permissions boundary of those users, so that nothing they are granted
	// can exceed it. Unset means no boundary.
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^arn:[a-z-]+:iam::([0-9]{12}|aws):policy/[!-~]+$`
	// +optional
	PermissionsBoundaryARN string `json:"permissionsBoundaryARN,omitempty"`
}

// IAMPathOf is the path of the IAM users: the store's, DefaultIAMPath when it
// names none.
func (t IAMTarget) IAMPathOf() string {
	if t.IAMPath == "" {
		return DefaultIAMPath
	}
	return t.IAMPath
}

// ClusterAWSBlock is provider.aws of a ClusterObjectStore: the common block of
// lib-operator-go with its endpoint override, and the IAM target.
type ClusterAWSBlock struct {
	cloud.AWSClusterBlock `json:",inline"`
	IAMTarget             `json:",inline"`
}

// NamespacedAWSBlock is provider.aws of an ObjectStore: the common block of
// lib-operator-go (it has no endpoint override), and the IAM target.
type NamespacedAWSBlock struct {
	cloud.AWSBlock `json:",inline"`
	IAMTarget      `json:",inline"`
}
