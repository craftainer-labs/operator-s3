package v1alpha1

import (
	"testing"
	"time"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
	"github.com/craftainer-labs/lib-operator-go/pkg/lifecycle"
	"github.com/craftainer-labs/lib-operator-go/pkg/server"
)

// The claim kinds and server kinds must satisfy the interfaces the
// lib-operator-go reconciler is written against.
var (
	_ lifecycle.Claim = (*Bucket)(nil)
	_ lifecycle.Claim = (*BucketAccess)(nil)
	_ server.Server   = (*ObjectStore)(nil)
	_ server.Server   = (*ClusterObjectStore)(nil)
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
		ServerCommon:      common.ServerCommon{ProxyURL: "http://proxy:3128"},
		ObjectStoreConfig: ObjectStoreConfig{Endpoint: "https://rustfs:9000"},
	}}
	g.Expect(store.GetCommon().ProxyURL).To(Equal("http://proxy:3128"))
	g.Expect(store.GetEndpoint()).To(Equal("https://rustfs:9000"))
	g.Expect(store.GetAllowedNamespaces()).To(BeNil())
}

func TestClusterObjectStoreAccessors(t *testing.T) {
	g := NewWithT(t)
	store := &ClusterObjectStore{Spec: ClusterObjectStoreSpec{
		ServerCommon:      common.ServerCommon{ProxyURL: "http://proxy:3128"},
		ObjectStoreConfig: ObjectStoreConfig{Endpoint: "https://rustfs:9000"},
		AllowedNamespaces: common.AllowedNamespaces{From: common.NamespacesAll},
	}}
	g.Expect(store.GetCommon().ProxyURL).To(Equal("http://proxy:3128"))
	g.Expect(store.GetEndpoint()).To(Equal("https://rustfs:9000"))
	g.Expect(store.GetAllowedNamespaces().From).To(Equal(common.NamespacesAll))
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
