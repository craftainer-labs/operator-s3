package v1alpha1

import (
	"testing"
	"time"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
	"github.com/craftainer-labs/lib-operator-go/pkg/cloud"
	"github.com/craftainer-labs/lib-operator-go/pkg/lifecycle"
	"github.com/craftainer-labs/lib-operator-go/pkg/probe"
	"github.com/craftainer-labs/lib-operator-go/pkg/server"
)

// The claim kinds and server kinds must satisfy the interfaces the
// lib-operator-go reconciler is written against.
var (
	_ lifecycle.Claim    = (*Bucket)(nil)
	_ lifecycle.Claim    = (*BucketAccess)(nil)
	_ server.CloudServer = (*ObjectStore)(nil)
	_ server.CloudServer = (*ClusterObjectStore)(nil)
	_ probe.CloudServer  = (*ObjectStore)(nil)
	_ probe.CloudServer  = (*ClusterObjectStore)(nil)
)

func TestAddToSchemeRegistersEveryKind(t *testing.T) {
	g := NewWithT(t)
	s := runtime.NewScheme()
	g.Expect(AddToScheme(s)).To(Succeed())
	for _, kind := range []string{
		"ClusterObjectStore", "ClusterObjectStoreList", "ObjectStore", "ObjectStoreList",
		"Bucket", "BucketList", "BucketAccess", "BucketAccessList",
	} {
		g.Expect(s.Recognizes(GroupVersion.WithKind(kind))).To(BeTrue(), kind)
	}
	g.Expect(GroupVersion.Group).To(Equal("s3.craftainer.dev"))
}

func TestObjectStoreAccessors(t *testing.T) {
	g := NewWithT(t)
	store := &ObjectStore{Spec: ObjectStoreSpec{
		CloudServerCommon: common.CloudServerCommon{ProxyURL: "http://proxy:3128"},
		ObjectStoreConfig: ObjectStoreConfig{Endpoint: "https://rustfs:9000"},
	}}
	g.Expect(store.GetCloudCommon().ProxyURL).To(Equal("http://proxy:3128"))
	g.Expect(store.GetEndpoint()).To(Equal("https://rustfs:9000"))
	g.Expect(store.GetAllowedNamespaces()).To(BeNil())

	// The probe writes through the pointer, into the kind's own status.
	store.GetServerStatus().ServerVersion = "1.0.0-rc.5"
	g.Expect(store.Status.ServerVersion).To(Equal("1.0.0-rc.5"))
}

func TestClusterObjectStoreAccessors(t *testing.T) {
	g := NewWithT(t)
	store := &ClusterObjectStore{Spec: ClusterObjectStoreSpec{
		CloudServerCommon: common.CloudServerCommon{ProxyURL: "http://proxy:3128"},
		ObjectStoreConfig: ObjectStoreConfig{Endpoint: "https://rustfs:9000"},
		AllowedNamespaces: common.AllowedNamespaces{From: common.NamespacesAll},
	}}
	g.Expect(store.GetCloudCommon().ProxyURL).To(Equal("http://proxy:3128"))
	g.Expect(store.GetEndpoint()).To(Equal("https://rustfs:9000"))
	g.Expect(store.GetAllowedNamespaces().From).To(Equal(common.NamespacesAll))

	store.GetServerStatus().ServerVersion = "1.0.0-rc.4"
	g.Expect(store.Status.ServerVersion).To(Equal("1.0.0-rc.4"))
}

func TestStoreConfigControllerDefaults(t *testing.T) {
	g := NewWithT(t)
	var c ObjectStoreConfig
	g.Expect(c.RegionOf()).To(Equal("us-east-1"))
	g.Expect(c.PathStyleOf()).To(BeTrue())
	g.Expect(c.BackendOf()).To(Equal(BackendRustFS))

	off := false
	c = ObjectStoreConfig{Region: "eu-west-1", PathStyle: &off, Backend: BackendRustFS}
	g.Expect(c.RegionOf()).To(Equal("eu-west-1"))
	g.Expect(c.PathStyleOf()).To(BeFalse())
	g.Expect(c.BackendOf()).To(Equal(BackendRustFS))
}

func TestStoreProviderAndCredentialsAccessors(t *testing.T) {
	g := NewWithT(t)

	cluster := &ClusterObjectStore{}
	g.Expect(cluster.HasProvider()).To(BeFalse())
	name, ns := cluster.GetCredentialsSecret()
	g.Expect(name).To(BeEmpty())
	g.Expect(ns).To(BeEmpty())
	cluster.Spec.Provider = &ClusterStoreProvider{Type: cloud.ProviderAWS}
	g.Expect(cluster.HasProvider()).To(BeTrue())
	// Workload identity names no Secret, whatever else is set.
	cluster.Spec.Credentials = &cloud.Credentials{Source: cloud.SourceWorkloadIdentity}
	name, _ = cluster.GetCredentialsSecret()
	g.Expect(name).To(BeEmpty())
	cluster.Spec.Credentials = &cloud.Credentials{
		Source: cloud.SourceSecret, SecretRef: &cloud.SecretRef{Name: "aws", Namespace: "craftainer-system"},
	}
	name, ns = cluster.GetCredentialsSecret()
	g.Expect(name).To(Equal("aws"))
	g.Expect(ns).To(Equal("craftainer-system"))

	store := &ObjectStore{}
	g.Expect(store.HasProvider()).To(BeFalse())
	name, ns = store.GetCredentialsSecret()
	g.Expect(name).To(BeEmpty())
	g.Expect(ns).To(BeEmpty())
	store.Spec.Provider = &NamespacedStoreProvider{Type: cloud.ProviderAWS}
	g.Expect(store.HasProvider()).To(BeTrue())
	store.Spec.Credentials = &cloud.NamespacedCredentials{Source: cloud.SourceSecret, SecretRef: &cloud.SecretRef{Name: "aws"}}
	name, ns = store.GetCredentialsSecret()
	g.Expect(name).To(Equal("aws"))
	g.Expect(ns).To(BeEmpty())
}

func TestBucketAccessors(t *testing.T) {
	g := NewWithT(t)
	b := &Bucket{Spec: BucketSpec{
		ClaimCommon: common.ClaimCommon{
			ServerRef:          common.ServerRef{Kind: "ObjectStore", Name: "store"},
			ManagementPolicies: []common.ManagementPolicy{common.ManagementObserve},
		},
		DataClaim: common.DataClaim{DeletionPolicy: common.DeletionDelete},
	}}
	g.Expect(b.GetServerRef()).To(Equal(common.ServerRef{Kind: "ObjectStore", Name: "store"}))
	g.Expect(b.GetDeletionPolicy()).To(Equal(common.DeletionDelete))
	g.Expect(b.GetManagementPolicies()).To(Equal([]common.ManagementPolicy{common.ManagementObserve}))

	*b.GetConditions() = append(*b.GetConditions(), metav1.Condition{Type: "Ready"})
	g.Expect(b.Status.Conditions).To(HaveLen(1))

	g.Expect(b.GetBindingSecretName()).To(BeEmpty())
	b.SetBindingName("ignored")
	g.Expect(b.Status.ClaimStatus.ExternalName).To(BeEmpty())

	b.SetExternalName("team-a-uploads")
	g.Expect(b.GetExternalName()).To(Equal("team-a-uploads"))
	b.SetExternalID("id-1")
	g.Expect(b.GetExternalID()).To(Equal("id-1"))
	b.SetObservedGeneration(3)
	g.Expect(b.Status.ObservedGeneration).To(Equal(int64(3)))
	now := metav1.NewTime(time.Unix(1700000000, 0))
	b.SetLastReconcileTime(now)
	g.Expect(b.Status.LastReconcileTime.Equal(&now)).To(BeTrue())

	b.SetEffectiveDeletionPolicy(common.DeletionRetain, common.SourceOperatorDefault)
	g.Expect(b.GetEffectiveDeletionPolicy()).To(Equal(common.DeletionRetain))
	g.Expect(b.Status.DeletionPolicySource).To(Equal(common.SourceOperatorDefault))

	b.SetLateInitialized(map[string]string{"k": "v"})
	g.Expect(b.Status.LateInitialized).To(HaveKeyWithValue("k", "v"))

	g.Expect(b.GetRotationPolicy()).To(BeNil())
	b.SetRotationStatus(common.RotationStatus{Active: "ignored"})
	g.Expect(b.GetRotationStatus()).To(Equal(common.RotationStatus{}))
}

func TestBucketAccessAccessors(t *testing.T) {
	g := NewWithT(t)
	rotation := &common.RotationPolicy{}
	a := &BucketAccess{
		ObjectMeta: metav1.ObjectMeta{Name: "uploads-app"},
		Spec: BucketAccessSpec{
			ClaimCommon: common.ClaimCommon{
				ServerRef:          common.ServerRef{Kind: "ClusterObjectStore", Name: "rustfs"},
				ManagementPolicies: []common.ManagementPolicy{common.ManagementAll},
			},
			Credentials: common.Credentials{RotationPolicy: rotation},
		},
	}
	g.Expect(a.GetServerRef()).To(Equal(common.ServerRef{Kind: "ClusterObjectStore", Name: "rustfs"}))
	g.Expect(a.GetDeletionPolicy()).To(BeEmpty())
	g.Expect(a.GetManagementPolicies()).To(Equal([]common.ManagementPolicy{common.ManagementAll}))

	*a.GetConditions() = append(*a.GetConditions(), metav1.Condition{Type: "Synced"})
	g.Expect(a.Status.Conditions).To(HaveLen(1))

	// The binding Secret is named after the claim unless the spec names it.
	g.Expect(a.GetBindingSecretName()).To(Equal("uploads-app"))
	a.Spec.Binding.SecretName = "uploads-s3"
	g.Expect(a.GetBindingSecretName()).To(Equal("uploads-s3"))
	a.SetBindingName("uploads-s3")
	g.Expect(a.Status.Binding.Name).To(Equal("uploads-s3"))

	a.SetExternalName("team-a-uploads-app")
	g.Expect(a.GetExternalName()).To(Equal("team-a-uploads-app"))
	a.SetExternalID("key-1")
	g.Expect(a.GetExternalID()).To(Equal("key-1"))
	a.SetObservedGeneration(4)
	g.Expect(a.Status.ObservedGeneration).To(Equal(int64(4)))
	now := metav1.NewTime(time.Unix(1700000000, 0))
	a.SetLastReconcileTime(now)
	g.Expect(a.Status.LastReconcileTime.Equal(&now)).To(BeTrue())

	a.SetEffectiveDeletionPolicy(common.DeletionRetain, common.SourceFallback)
	g.Expect(a.GetEffectiveDeletionPolicy()).To(BeEmpty())

	a.SetLateInitialized(map[string]string{"k": "v"})
	g.Expect(a.Status.LateInitialized).To(HaveKeyWithValue("k", "v"))

	g.Expect(a.GetRotationPolicy()).To(BeIdenticalTo(rotation))
	g.Expect(a.GetRotationStatus()).To(Equal(common.RotationStatus{}))
	a.SetRotationStatus(common.RotationStatus{Active: "g1", Sequence: 2})
	g.Expect(a.GetRotationStatus()).To(Equal(common.RotationStatus{Active: "g1", Sequence: 2}))
}

func TestPrefixOK(t *testing.T) {
	g := NewWithT(t)
	for _, ok := range []string{"", "team/a/", "a b", "a-b_c.d"} {
		g.Expect(PrefixOK(ok)).To(BeTrue(), ok)
	}
	for _, bad := range []string{"*", "a/*", "a?b", "${aws:username}/", "$"} {
		g.Expect(PrefixOK(bad)).To(BeFalse(), bad)
	}
}
