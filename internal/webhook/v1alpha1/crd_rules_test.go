package v1alpha1

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
	s3v1alpha1 "github.com/craftainer-labs/operator-s3/api/v1alpha1"
)

// These specs send objects through the API server with the generated CRDs
// installed, so they check the schema defaults and the CEL rules next to the
// webhooks: the CRDs are the API's contract and the webhooks only add to it.

var _ = Describe("The s3.craftainer.dev CRDs", func() {
	const ns = "default"

	// create creates obj and deletes it when the spec ends.
	create := func(obj client.Object) error {
		err := k8sClient.Create(ctx, obj)
		if err == nil {
			DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed()) })
		}
		return err
	}
	rejected := func(err error, substring string) {
		GinkgoHelper()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(substring))
	}

	clusterStore := func(name string) *s3v1alpha1.ClusterObjectStore {
		return &s3v1alpha1.ClusterObjectStore{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: s3v1alpha1.ClusterObjectStoreSpec{
				ServerCommon: common.ServerCommon{
					AdminCredentialsSecretRef: common.AdminSecretRef{Name: "admin", Namespace: "craftainer-system"},
				},
				ObjectStoreConfig: s3v1alpha1.ObjectStoreConfig{Endpoint: "https://rustfs:9000"},
				AllowedNamespaces: common.AllowedNamespaces{From: common.NamespacesAll},
			},
		}
	}
	store := func(name string) *s3v1alpha1.ObjectStore {
		return &s3v1alpha1.ObjectStore{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: s3v1alpha1.ObjectStoreSpec{
				ServerCommon:      common.ServerCommon{AdminCredentialsSecretRef: common.AdminSecretRef{Name: "admin"}},
				ObjectStoreConfig: s3v1alpha1.ObjectStoreConfig{Endpoint: "https://rustfs:9000"},
			},
		}
	}
	bucket := func(name string) *s3v1alpha1.Bucket {
		return &s3v1alpha1.Bucket{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: s3v1alpha1.BucketSpec{
				ClaimCommon: common.ClaimCommon{ServerRef: common.ServerRef{Kind: "ObjectStore", Name: "store"}},
			},
		}
	}
	access := func(name string) *s3v1alpha1.BucketAccess {
		return &s3v1alpha1.BucketAccess{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: s3v1alpha1.BucketAccessSpec{
				ClaimCommon: common.ClaimCommon{ServerRef: common.ServerRef{Kind: "ObjectStore", Name: "store"}},
				BucketRef:   s3v1alpha1.BucketRef{Name: "uploads"},
			},
		}
	}

	Describe("ClusterObjectStore", func() {
		It("applies the schema defaults", func() {
			Expect(create(clusterStore("defaults"))).To(Succeed())
			got := &s3v1alpha1.ClusterObjectStore{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "defaults"}, got)).To(Succeed())
			Expect(got.Spec.Region).To(Equal("us-east-1"))
			Expect(got.Spec.Backend).To(Equal(s3v1alpha1.BackendRustFS))
			Expect(got.Spec.PathStyle).To(HaveValue(BeTrue()))
		})

		It("keeps an explicit pathStyle false", func() {
			s := clusterStore("virtual-hosted")
			off := false
			s.Spec.PathStyle = &off
			Expect(create(s)).To(Succeed())
			got := &s3v1alpha1.ClusterObjectStore{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "virtual-hosted"}, got)).To(Succeed())
			Expect(got.Spec.PathStyle).To(HaveValue(BeFalse()))
		})

		It("requires the admin Secret's namespace", func() {
			s := clusterStore("no-ns")
			s.Spec.AdminCredentialsSecretRef.Namespace = ""
			rejected(create(s), "adminCredentialsSecretRef.namespace is required")
		})

		It("fails closed without a valid allowedNamespaces", func() {
			s := clusterStore("no-from")
			s.Spec.AllowedNamespaces = common.AllowedNamespaces{}
			rejected(create(s), "allowedNamespaces")

			s = clusterStore("selector-missing")
			s.Spec.AllowedNamespaces = common.AllowedNamespaces{From: common.NamespacesSelector}
			rejected(create(s), "selector is required when from is Selector")

			s = clusterStore("selector-empty")
			s.Spec.AllowedNamespaces = common.AllowedNamespaces{From: common.NamespacesSelector, Selector: &common.LabelSelector{}}
			rejected(create(s), "selector must have matchLabels or matchExpressions")

			s = clusterStore("selector-with-all")
			s.Spec.AllowedNamespaces.Selector = &common.LabelSelector{MatchLabels: map[string]string{"a": "b"}}
			rejected(create(s), "selector must not be set when from is All")
		})

		It("admits a selector that matches labels", func() {
			s := clusterStore("selector")
			s.Spec.AllowedNamespaces = common.AllowedNamespaces{
				From:     common.NamespacesSelector,
				Selector: &common.LabelSelector{MatchLabels: map[string]string{"craftainer.dev/s3": "allowed"}},
			}
			Expect(create(s)).To(Succeed())
		})

		It("rejects a plain http endpoint", func() {
			s := clusterStore("http")
			s.Spec.Endpoint = "http://rustfs:9000"
			rejected(create(s), "endpoint must be an https URL")
		})

		It("rejects a proxy URL with an unsupported scheme in the webhook", func() {
			s := clusterStore("bad-proxy")
			s.Spec.ProxyURL = "ftp://proxy"
			rejected(create(s), "must be an http, https or socks5 URL with a host")
		})
	})

	Describe("ObjectStore", func() {
		It("is admitted with the Secret in its own namespace", func() {
			Expect(create(store("plain"))).To(Succeed())
		})

		It("forbids a namespace on the admin Secret", func() {
			s := store("with-ns")
			s.Spec.AdminCredentialsSecretRef.Namespace = "elsewhere"
			rejected(create(s), "adminCredentialsSecretRef.namespace must not be set")
		})

		It("rejects an endpoint without a host in the webhook", func() {
			s := store("no-host")
			s.Spec.Endpoint = "https://"
			rejected(create(s), "must be an https URL with a host")
		})
	})

	Describe("Bucket", func() {
		It("applies the schema defaults and leaves deletionPolicy to the operator", func() {
			Expect(create(bucket("defaults"))).To(Succeed())
			got := &s3v1alpha1.Bucket{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "defaults"}, got)).To(Succeed())
			Expect(got.Spec.ManagementPolicies).To(Equal([]common.ManagementPolicy{common.ManagementAll}))
			Expect(got.Spec.Versioning).To(BeFalse())
			Expect(got.Spec.ObjectLock).To(BeFalse())
			Expect(got.Spec.DeletionPolicy).To(BeEmpty())
		})

		It("rejects a name with a dot", func() {
			rejected(create(bucket("my.bucket")), "metadata.name must not contain dots")
		})

		It("rejects a server kind of another group", func() {
			b := bucket("other-kind")
			b.Spec.ServerRef.Kind = "DatabaseServer"
			rejected(create(b), "serverRef.kind must be ClusterObjectStore or ObjectStore")
		})

		It("keeps objectLock and serverRef immutable", func() {
			b := bucket("locked")
			b.Spec.ObjectLock = true
			Expect(create(b)).To(Succeed())

			b.Spec.ObjectLock = false
			rejected(k8sClient.Update(ctx, b), "objectLock is immutable")

			fresh := &s3v1alpha1.Bucket{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(b), fresh)).To(Succeed())
			fresh.Spec.ServerRef.Name = "other"
			rejected(k8sClient.Update(ctx, fresh), "serverRef is immutable")
		})

		It("checks the lifecycle rules", func() {
			b := bucket("lifecycle")
			b.Spec.LifecycleRules = []s3v1alpha1.LifecycleRule{
				{ID: "tmp", Prefix: "tmp/", ExpirationDays: 7},
				{ID: "tmp", Prefix: "logs/", ExpirationDays: 30},
			}
			rejected(create(b), "Duplicate value")

			b = bucket("lifecycle")
			b.Spec.LifecycleRules = []s3v1alpha1.LifecycleRule{{ID: "zero", ExpirationDays: 0}}
			rejected(create(b), "expirationDays")

			b = bucket("lifecycle")
			b.Spec.LifecycleRules = []s3v1alpha1.LifecycleRule{{ID: "slash", Prefix: "/tmp", ExpirationDays: 1}}
			rejected(create(b), "prefix must not start with /")

			b = bucket("lifecycle")
			b.Spec.LifecycleRules = []s3v1alpha1.LifecycleRule{{ID: "ok", Prefix: "tmp/", ExpirationDays: 1}}
			Expect(create(b)).To(Succeed())
		})

		It("checks the quota in the webhook", func() {
			b := bucket("quota")
			b.Spec.Quota = &s3v1alpha1.BucketQuota{HardBytes: resource.MustParse("0")}
			rejected(create(b), "must be at least 1 byte")

			b = bucket("quota")
			b.Spec.Quota = &s3v1alpha1.BucketQuota{HardBytes: resource.MustParse("50Gi")}
			Expect(create(b)).To(Succeed())
		})
	})

	Describe("BucketAccess", func() {
		It("applies the schema defaults", func() {
			Expect(create(access("defaults"))).To(Succeed())
			got := &s3v1alpha1.BucketAccess{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "defaults"}, got)).To(Succeed())
			Expect(got.Spec.Access).To(Equal(s3v1alpha1.AccessReadWrite))
			Expect(got.Spec.ManagementPolicies).To(Equal([]common.ManagementPolicy{common.ManagementAll}))
			Expect(got.Spec.RotationPolicy).To(BeNil())
		})

		It("keeps bucketRef immutable", func() {
			a := access("pinned")
			Expect(create(a)).To(Succeed())
			a.Spec.BucketRef.Name = "other"
			rejected(k8sClient.Update(ctx, a), "bucketRef is immutable")
		})

		It("rejects a prefix with a leading slash and a foreign server kind", func() {
			a := access("slash")
			a.Spec.Prefix = "/app"
			rejected(create(a), "prefix must not start with /")

			a = access("other-kind")
			a.Spec.ServerRef.Kind = "MqttBroker"
			rejected(create(a), "serverRef.kind must be ClusterObjectStore or ObjectStore")
		})

		It("needs the Update management policy for a rotationPolicy", func() {
			a := access("rotating")
			a.Spec.ManagementPolicies = []common.ManagementPolicy{common.ManagementObserve, common.ManagementCreate}
			a.Spec.RotationPolicy = &common.RotationPolicy{Interval: &metav1.Duration{Duration: 2160 * time.Hour}}
			rejected(create(a), "rotationPolicy needs the Update management policy")

			a = access("rotating")
			a.Spec.RotationPolicy = &common.RotationPolicy{Interval: &metav1.Duration{Duration: 2160 * time.Hour}}
			Expect(create(a)).To(Succeed())
		})
	})
})
