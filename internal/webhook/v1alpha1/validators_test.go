package v1alpha1

import (
	"context"
	"net/url"
	"testing"

	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
	s3v1alpha1 "github.com/craftainer-labs/operator-s3/api/v1alpha1"
)

func storeConfig(endpoint string) s3v1alpha1.ObjectStoreConfig {
	return s3v1alpha1.ObjectStoreConfig{Endpoint: endpoint}
}

func objectStore(endpoint, proxy string) *s3v1alpha1.ObjectStore {
	return &s3v1alpha1.ObjectStore{
		ObjectMeta: metav1.ObjectMeta{Name: "store", Namespace: "default"},
		Spec: s3v1alpha1.ObjectStoreSpec{
			ServerCommon:      common.ServerCommon{ProxyURL: proxy},
			ObjectStoreConfig: storeConfig(endpoint),
		},
	}
}

func clusterObjectStore(endpoint, proxy string) *s3v1alpha1.ClusterObjectStore {
	return &s3v1alpha1.ClusterObjectStore{
		ObjectMeta: metav1.ObjectMeta{Name: "store"},
		Spec: s3v1alpha1.ClusterObjectStoreSpec{
			ServerCommon:      common.ServerCommon{ProxyURL: proxy},
			ObjectStoreConfig: storeConfig(endpoint),
		},
	}
}

func bucketWithQuota(quota string) *s3v1alpha1.Bucket {
	b := &s3v1alpha1.Bucket{ObjectMeta: metav1.ObjectMeta{Name: "uploads", Namespace: "default"}}
	if quota != "" {
		b.Spec.Quota = &s3v1alpha1.BucketQuota{HardBytes: resource.MustParse(quota)}
	}
	return b
}

// withUser puts userinfo into a URL at run time, so the test data holds no
// credential-shaped literal.
func withUser(raw, user, password string) string {
	u, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	if password == "" {
		u.User = url.User(user)
	} else {
		u.User = url.UserPassword(user, password)
	}
	return u.String()
}

// storeCases are the URL rules, shared by both store kinds.
var storeCases = []struct {
	name, endpoint, proxy string
	field                 string // the field the error names; empty means valid
}{
	{name: "valid", endpoint: "https://rustfs.storage.svc:9000"},
	{name: "valid with a proxy", endpoint: "https://rustfs:9000", proxy: "socks5://proxy:1080"},
	{name: "http endpoint", endpoint: "http://rustfs:9000", field: "spec.endpoint"},
	{name: "endpoint without a host", endpoint: "https://", field: "spec.endpoint"},
	{name: "malformed endpoint", endpoint: "https://[::1", field: "spec.endpoint"},
	{name: "proxy with another scheme", endpoint: "https://rustfs:9000", proxy: "ftp://proxy", field: "spec.proxyURL"},
	{name: "proxy without a host", endpoint: "https://rustfs:9000", proxy: "http://", field: "spec.proxyURL"},
	{name: "endpoint with userinfo", endpoint: withUser("https://rustfs:9000", "user", "s3cret"), field: "spec.endpoint"},
	{name: "endpoint with a user only", endpoint: withUser("https://rustfs:9000", "user", ""), field: "spec.endpoint"},
	{name: "proxy with userinfo", endpoint: "https://rustfs:9000", proxy: withUser("http://proxy:3128", "user", "s3cret"), field: "spec.proxyURL"},
	{name: "socks5 proxy with a password", endpoint: "https://rustfs:9000", proxy: withUser("socks5://proxy:1080", "", "s3cret"), field: "spec.proxyURL"},
}

func expectField(g *WithT, err error, field string) {
	if field == "" {
		g.Expect(err).NotTo(HaveOccurred())
		return
	}
	g.Expect(apierrors.IsInvalid(err)).To(BeTrue(), "%v", err)
	g.Expect(err.Error()).To(ContainSubstring(field))
}

func TestObjectStoreValidator(t *testing.T) {
	v := &ObjectStoreValidator{}
	ctx := context.Background()
	for _, tc := range storeCases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			warnings, err := v.ValidateCreate(ctx, objectStore(tc.endpoint, tc.proxy))
			g.Expect(warnings).To(BeEmpty())
			expectField(g, err, tc.field)

			_, err = v.ValidateUpdate(ctx, objectStore("https://old:9000", ""), objectStore(tc.endpoint, tc.proxy))
			expectField(g, err, tc.field)
		})
	}

	g := NewWithT(t)
	// An object being deleted is always admitted, so finalizer removal is
	// never blocked.
	deleting := objectStore("http://bad", "")
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	_, err := v.ValidateUpdate(ctx, objectStore("https://old:9000", ""), deleting)
	g.Expect(err).NotTo(HaveOccurred())
	_, err = v.ValidateDelete(ctx, objectStore("http://bad", ""))
	g.Expect(err).NotTo(HaveOccurred())
}

func TestClusterObjectStoreValidator(t *testing.T) {
	v := &ClusterObjectStoreValidator{}
	ctx := context.Background()
	for _, tc := range storeCases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			warnings, err := v.ValidateCreate(ctx, clusterObjectStore(tc.endpoint, tc.proxy))
			g.Expect(warnings).To(BeEmpty())
			expectField(g, err, tc.field)

			_, err = v.ValidateUpdate(ctx, clusterObjectStore("https://old:9000", ""), clusterObjectStore(tc.endpoint, tc.proxy))
			expectField(g, err, tc.field)
		})
	}

	g := NewWithT(t)
	deleting := clusterObjectStore("http://bad", "")
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	_, err := v.ValidateUpdate(ctx, clusterObjectStore("https://old:9000", ""), deleting)
	g.Expect(err).NotTo(HaveOccurred())
	_, err = v.ValidateDelete(ctx, clusterObjectStore("http://bad", ""))
	g.Expect(err).NotTo(HaveOccurred())
}

func TestStoreErrorsDoNotEchoTheURL(t *testing.T) {
	g := NewWithT(t)
	_, err := (&ObjectStoreValidator{}).ValidateCreate(context.Background(),
		objectStore("https://user:hunter2@", "http://user:hunter2@"))
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).NotTo(ContainSubstring("hunter2"))
	g.Expect(err.Error()).To(And(ContainSubstring("spec.endpoint"), ContainSubstring("spec.proxyURL")))
}

func TestBucketValidator(t *testing.T) {
	v := &BucketValidator{}
	ctx := context.Background()
	for _, tc := range []struct {
		name, quota string
		valid       bool
	}{
		{name: "no quota", valid: true},
		{name: "one byte", quota: "1", valid: true},
		{name: "50Gi", quota: "50Gi", valid: true},
		{name: "zero", quota: "0"},
		{name: "less than a byte", quota: "500m"},
		{name: "negative", quota: "-1Gi"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			warnings, err := v.ValidateCreate(ctx, bucketWithQuota(tc.quota))
			g.Expect(warnings).To(BeEmpty())
			_, updateErr := v.ValidateUpdate(ctx, bucketWithQuota(""), bucketWithQuota(tc.quota))
			if tc.valid {
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(updateErr).NotTo(HaveOccurred())
				return
			}
			for _, e := range []error{err, updateErr} {
				g.Expect(apierrors.IsInvalid(e)).To(BeTrue(), "%v", e)
				g.Expect(e.Error()).To(ContainSubstring("spec.quota.hardBytes"))
			}
		})
	}

	g := NewWithT(t)
	deleting := bucketWithQuota("0")
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	_, err := v.ValidateUpdate(ctx, bucketWithQuota(""), deleting)
	g.Expect(err).NotTo(HaveOccurred())
	_, err = v.ValidateDelete(ctx, bucketWithQuota("0"))
	g.Expect(err).NotTo(HaveOccurred())
}

func bucketAccessWithPrefix(prefix string) *s3v1alpha1.BucketAccess {
	return &s3v1alpha1.BucketAccess{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
		Spec:       s3v1alpha1.BucketAccessSpec{Prefix: prefix},
	}
}

func TestBucketAccessValidator(t *testing.T) {
	v := &BucketAccessValidator{}
	ctx := context.Background()
	for _, tc := range []struct {
		prefix string
		valid  bool
	}{
		{prefix: "", valid: true},
		{prefix: "app/", valid: true},
		{prefix: "app/2026-11/", valid: true},
		{prefix: "*"},
		{prefix: "app/*"},
		{prefix: "a?b"},
		{prefix: "${aws:username}/"},
	} {
		t.Run(tc.prefix, func(t *testing.T) {
			g := NewWithT(t)
			warnings, err := v.ValidateCreate(ctx, bucketAccessWithPrefix(tc.prefix))
			g.Expect(warnings).To(BeEmpty())
			_, updateErr := v.ValidateUpdate(ctx, bucketAccessWithPrefix(""), bucketAccessWithPrefix(tc.prefix))
			if tc.valid {
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(updateErr).NotTo(HaveOccurred())
				return
			}
			for _, e := range []error{err, updateErr} {
				g.Expect(apierrors.IsInvalid(e)).To(BeTrue(), "%v", e)
				g.Expect(e.Error()).To(ContainSubstring("spec.prefix"))
			}
		})
	}

	g := NewWithT(t)
	deleting := bucketAccessWithPrefix("*")
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	_, err := v.ValidateUpdate(ctx, bucketAccessWithPrefix(""), deleting)
	g.Expect(err).NotTo(HaveOccurred())
	_, err = v.ValidateDelete(ctx, bucketAccessWithPrefix("*"))
	g.Expect(err).NotTo(HaveOccurred())
}
