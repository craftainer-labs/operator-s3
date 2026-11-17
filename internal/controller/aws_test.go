package controller

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
	"github.com/craftainer-labs/lib-operator-go/pkg/cloud"
	"github.com/craftainer-labs/lib-operator-go/pkg/conditions"
	"github.com/craftainer-labs/lib-operator-go/pkg/egress"
	"github.com/craftainer-labs/lib-operator-go/pkg/server"

	s3v1alpha1 "github.com/craftainer-labs/operator-s3/api/v1alpha1"
	"github.com/craftainer-labs/operator-s3/internal/awss3"
	"github.com/craftainer-labs/operator-s3/internal/s3client"
)

const awsCredsName = "aws-creds"

func awsKeys(access string) map[string][]byte {
	return map[string][]byte{cloud.KeyAWSAccessKeyID: []byte(access), cloud.KeyAWSSecretAccessKey: []byte("aws-secret")}
}

// newAWSHarness is a harness whose cluster store runs on AWS in eu-west-1 with
// the credentials of a Secret in the operator namespace.
func newAWSHarness(t *testing.T, mutate func(*s3v1alpha1.ClusterObjectStore)) *harness {
	t.Helper()
	h := newHarnessOps(t, common.DeletionRetain, func(ops string, s *s3v1alpha1.ClusterObjectStore) {
		s.Spec.Endpoint, s.Spec.AdminCredentialsSecretRef = "", nil
		s.Spec.Provider = &s3v1alpha1.ClusterStoreProvider{
			Type: cloud.ProviderAWS, AWS: &cloud.AWSClusterBlock{AWSBlock: cloud.AWSBlock{Region: "eu-west-1"}},
		}
		s.Spec.Credentials = &cloud.Credentials{Source: cloud.SourceSecret, SecretRef: &cloud.SecretRef{Name: awsCredsName, Namespace: ops}}
		if mutate != nil {
			mutate(s)
		}
	})
	h.must(env.Client.Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: awsCredsName, Namespace: h.ops}, Data: awsKeys("AKIAOPERATOR"),
	}))
	h.cfg.Cache = NewStoreCache(time.Hour)
	return h
}

func retrieved(t *testing.T, cfg aws.Config) aws.Credentials {
	t.Helper()
	c, err := cfg.Credentials.Retrieve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The Store of a store on AWS is built from the credentials Secret and the
// region of the provider, and kept until one of them changes.
func TestBackendBuildsTheAWSStore(t *testing.T) {
	h := newAWSHarness(t, nil)
	ctx := context.Background()

	b, err := h.cfg.backend(ctx, h.ns, h.ref())
	if err != nil || b.store != Store(h.fake) {
		t.Fatalf("%v, %v", b, err)
	}
	if len(h.fake.awsConfigs) != 1 {
		t.Fatalf("%d configurations", len(h.fake.awsConfigs))
	}
	cfg := h.fake.awsConfigs[0]
	if cfg.Region != "eu-west-1" || cfg.BaseEndpoint != nil || retrieved(t, cfg).AccessKeyID != "AKIAOPERATOR" {
		t.Errorf("configuration = %+v", cfg)
	}
	if b.region() != "eu-west-1" || b.host != "s3.eu-west-1.amazonaws.com" || len(h.fake.targets) != 0 {
		t.Errorf("region %q, host %q, %d RustFS clients", b.region(), b.host, len(h.fake.targets))
	}

	// Nothing changed: the Store is reused.
	if _, err = h.cfg.backend(ctx, h.ns, h.ref()); err != nil || len(h.fake.awsConfigs) != 1 {
		t.Errorf("reuse: %v, %d configurations", err, len(h.fake.awsConfigs))
	}
	// New credentials in the Secret: a new Store.
	h.must(env.Client.Update(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: awsCredsName, Namespace: h.ops}, Data: awsKeys("AKIAROTATED"),
	}))
	if _, err = h.cfg.backend(ctx, h.ns, h.ref()); err != nil || len(h.fake.awsConfigs) != 2 ||
		retrieved(t, h.fake.awsConfigs[1]).AccessKeyID != "AKIAROTATED" {
		t.Errorf("rotated: %v, %d configurations", err, len(h.fake.awsConfigs))
	}
}

// A namespaced store has its own credentials Secret and no override.
func TestBackendBuildsTheAWSStoreOfANamespacedStore(t *testing.T) {
	h := newAWSHarness(t, nil)
	ctx := context.Background()
	h.must(env.Client.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: awsCredsName, Namespace: h.ns}, Data: awsKeys("AKIATENANT"),
	}))
	st := &s3v1alpha1.ObjectStore{ObjectMeta: metav1.ObjectMeta{Name: "mine", Namespace: h.ns}}
	st.Spec.Provider = &s3v1alpha1.NamespacedStoreProvider{Type: cloud.ProviderAWS, AWS: &cloud.AWSBlock{Region: "ap-southeast-2"}}
	st.Spec.Credentials = &cloud.NamespacedCredentials{Source: cloud.SourceSecret, SecretRef: &cloud.SecretRef{Name: awsCredsName}}
	h.must(env.Client.Create(ctx, st))

	b, err := h.cfg.backend(ctx, h.ns, common.ServerRef{Kind: s3v1alpha1.KindObjectStore, Name: "mine"})
	if err != nil || len(h.fake.awsConfigs) != 1 {
		t.Fatalf("%v, %v", b, err)
	}
	cfg := h.fake.awsConfigs[0]
	if cfg.Region != "ap-southeast-2" || retrieved(t, cfg).AccessKeyID != "AKIATENANT" || b.region() != "ap-southeast-2" {
		t.Errorf("configuration = %+v", cfg)
	}
}

// The endpoint override is for tests of the operator with EGRESS_MODE=allow-private.
func TestBackendEndpointOverride(t *testing.T) {
	ctx := context.Background()
	override := func(url string) func(*s3v1alpha1.ClusterObjectStore) {
		return func(s *s3v1alpha1.ClusterObjectStore) { s.Spec.Provider.AWS.EndpointOverride = url }
	}

	// Strict: refused, before anything is built.
	h := newAWSHarness(t, override("http://10.1.2.3:5000"))
	_, err := h.cfg.backend(ctx, h.ns, h.ref())
	if !errors.Is(err, server.ErrInvalidServer) || !errors.Is(err, cloud.ErrEndpointOverrideForbidden) || len(h.fake.awsConfigs) != 0 {
		t.Errorf("strict: %v", err)
	}

	// Allow-private: honoured, and the host is the override's.
	h = newAWSHarness(t, override("http://10.1.2.3:5000"))
	h.cfg.EgressMode = egress.ModeAllowPrivate
	b, err := h.cfg.backend(ctx, h.ns, h.ref())
	if err != nil || b.host != "10.1.2.3:5000" || len(h.fake.awsConfigs) != 1 || *h.fake.awsConfigs[0].BaseEndpoint != "http://10.1.2.3:5000" {
		t.Errorf("allow-private: %+v, %v", b, err)
	}

	// An override that is no URL passes the schema's pattern and is refused
	// when the configuration is built.
	h = newAWSHarness(t, override("http://[::1"))
	h.cfg.EgressMode = egress.ModeAllowPrivate
	b, err = h.cfg.backend(ctx, h.ns, h.ref())
	if !errors.Is(err, cloud.ErrInvalidEndpoint) || b != nil {
		t.Errorf("bad override: %+v, %v", b, err)
	}
}

// A store whose credentials cannot be had says so without the keys.
func TestBackendCredentialsFailures(t *testing.T) {
	ctx := context.Background()

	// The Secret lacks a key.
	h := newAWSHarness(t, nil)
	h.must(env.Client.Update(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: awsCredsName, Namespace: h.ops},
		Data:       map[string][]byte{cloud.KeyAWSAccessKeyID: []byte("AKIAONLY")},
	}))
	_, err := h.cfg.backend(ctx, h.ns, h.ref())
	if !errors.Is(err, server.ErrInvalidServer) || !errors.Is(err, cloud.ErrCredentialsUnavailable) || strings.Contains(err.Error(), "AKIAONLY") {
		t.Errorf("missing key: %v", err)
	}
	if h.cfg.Cache.Len() != 0 {
		t.Error("a failed build was cached")
	}

	// No Secret.
	h = newAWSHarness(t, nil)
	h.must(env.Client.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: awsCredsName, Namespace: h.ops}}))
	if _, err = h.cfg.backend(ctx, h.ns, h.ref()); !errors.Is(err, server.ErrCredentialsUnavailable) {
		t.Errorf("missing Secret: %v", err)
	}
}

// A provider that is not AWS, or whose block is missing, is refused before any
// client is built. The CRD's rules keep both out; this is the guard behind
// them.
func TestStoreWithAnotherProviderIsRefused(t *testing.T) {
	cluster := func(p *s3v1alpha1.ClusterStoreProvider) *s3v1alpha1.ClusterObjectStore {
		return &s3v1alpha1.ClusterObjectStore{Spec: s3v1alpha1.ClusterObjectStoreSpec{Provider: p}}
	}
	namespaced := func(p *s3v1alpha1.NamespacedStoreProvider) *s3v1alpha1.ObjectStore {
		return &s3v1alpha1.ObjectStore{Spec: s3v1alpha1.ObjectStoreSpec{Provider: p}}
	}
	p, targets := probeFixture(&versionStore{})
	for name, srv := range map[string]server.CloudServer{
		"cluster, GCP":         cluster(&s3v1alpha1.ClusterStoreProvider{Type: cloud.ProviderGCP}),
		"cluster, no block":    cluster(&s3v1alpha1.ClusterStoreProvider{Type: cloud.ProviderAWS}),
		"namespaced, GCP":      namespaced(&s3v1alpha1.NamespacedStoreProvider{Type: cloud.ProviderGCP}),
		"namespaced, no block": namespaced(&s3v1alpha1.NamespacedStoreProvider{Type: cloud.ProviderAWS}),
	} {
		_, err := probeVersion(context.Background(), p, srv, probeConn())
		if !errors.Is(err, ErrCloudStore) || !errors.Is(err, server.ErrInvalidServer) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(*targets) != 0 {
		t.Errorf("%d clients built", len(*targets))
	}
}

// A store that names a provider the object cannot be fetched with is refused
// by the claims too.
func TestBackendRefusesAProviderWithoutABlock(t *testing.T) {
	// The CRD admits no such store, so the object is read through a client that
	// hands it out.
	h := newAWSHarness(t, nil)
	h.cfg.Client = &providerlessClient{Client: h.cfg.Client}
	if _, err := h.cfg.backend(context.Background(), h.ns, h.ref()); !errors.Is(err, ErrCloudStore) || !errors.Is(err, server.ErrInvalidServer) {
		t.Errorf("cluster: %v", err)
	}
	for _, kind := range []string{s3v1alpha1.KindObjectStore, s3v1alpha1.KindClusterObjectStore} {
		if _, err := h.cfg.fetchStore(context.Background(), h.ns, common.ServerRef{Kind: kind, Name: "absent"}); err == nil {
			t.Errorf("%s: a missing store was found", kind)
		}
	}
}

// providerlessClient hands out a ClusterObjectStore whose AWS block is gone,
// which the CRD's rules do not admit.
type providerlessClient struct{ client.Client }

func (c *providerlessClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	err := c.Client.Get(ctx, key, obj, opts...)
	if s, ok := obj.(*s3v1alpha1.ClusterObjectStore); ok && err == nil && s.Spec.Provider != nil {
		s.Spec.Provider.AWS = nil
	}
	return err
}

// The AWS probe, with the result it discovers.
func awsProber(probed *[]aws.Config, probeErr error, mode egress.Mode) Prober {
	return Prober{Config: Config{
		Egress: egress.NewPolicy(mode), EgressMode: mode,
		ProbeAWS: func(_ context.Context, cfg aws.Config) error {
			*probed = append(*probed, cfg)
			return probeErr
		},
	}}
}

func awsCluster(region, override string) *s3v1alpha1.ClusterObjectStore {
	s := &s3v1alpha1.ClusterObjectStore{}
	s.Spec.Provider = &s3v1alpha1.ClusterStoreProvider{
		Type: cloud.ProviderAWS, AWS: &cloud.AWSClusterBlock{AWSBlock: cloud.AWSBlock{Region: region}, EndpointOverride: override},
	}
	s.Spec.Credentials = &cloud.Credentials{Source: cloud.SourceSecret}
	return s
}

func awsConn() server.CloudConnection {
	return server.CloudConnection{Credentials: awsKeys("AKIAPROBE")}
}

// Only the region and the endpoint are discovered; the account and the
// identity are not read, and the version stays empty.
func TestProberDiscoversTheRegionAndEndpoint(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		srv                   server.CloudServer
		mode                  egress.Mode
		region, endpoint, key string
	}{
		"cluster":   {awsCluster("us-west-2", ""), egress.ModeStrict, "us-west-2", "https://s3.us-west-2.amazonaws.com", "AKIAPROBE"},
		"china":     {awsCluster("cn-north-1", ""), egress.ModeStrict, "cn-north-1", "https://s3.cn-north-1.amazonaws.com.cn", "AKIAPROBE"},
		"emulator":  {awsCluster("us-east-1", "http://10.1.2.3:5000"), egress.ModeAllowPrivate, "us-east-1", "http://10.1.2.3:5000", "AKIAPROBE"},
		"namespace": {awsNamespaced("eu-north-1"), egress.ModeStrict, "eu-north-1", "https://s3.eu-north-1.amazonaws.com", "AKIAPROBE"},
	} {
		var probed []aws.Config
		res, err := awsProber(&probed, nil, tc.mode).ProbeCloud(ctx, tc.srv, awsConn())
		if err != nil || res.Version != "" || len(res.Discovered) != 2 ||
			res.Discovered[DiscoveredRegion] != tc.region || res.Discovered[DiscoveredEndpoint] != tc.endpoint {
			t.Errorf("%s: %+v, %v", name, res, err)
		}
		if len(probed) != 1 || probed[0].Region != tc.region || retrieved(t, probed[0]).AccessKeyID != tc.key {
			t.Errorf("%s: probed %+v", name, probed)
		}
	}
}

func awsNamespaced(region string) *s3v1alpha1.ObjectStore {
	s := &s3v1alpha1.ObjectStore{}
	s.Spec.Provider = &s3v1alpha1.NamespacedStoreProvider{Type: cloud.ProviderAWS, AWS: &cloud.AWSBlock{Region: region}}
	s.Spec.Credentials = &cloud.NamespacedCredentials{Source: cloud.SourceSecret}
	return s
}

// A failed probe is shown as unreachable, never as an invalid server, and
// carries no credentials.
func TestAWSProbeFailures(t *testing.T) {
	ctx := context.Background()
	var probed []aws.Config

	probeErr := errors.Join(s3client.ErrConnAuth, errors.New("denied"))
	res, err := awsProber(&probed, probeErr, egress.ModeStrict).ProbeCloud(ctx, awsCluster("us-east-1", ""), awsConn())
	if !errors.Is(err, s3client.ErrConnAuth) || errors.Is(err, server.ErrInvalidServer) || res.Discovered != nil {
		t.Errorf("probe: %+v, %v", res, err)
	}

	// An override in strict mode, and a namespaced store on the operator's
	// identity, are refused before anything is asked of AWS.
	probed = nil
	for name, tc := range map[string]struct {
		srv  server.CloudServer
		want error
	}{
		"override": {awsCluster("us-east-1", "http://10.1.2.3:5000"), cloud.ErrEndpointOverrideForbidden},
		"identity": {func() server.CloudServer {
			s := awsNamespaced("us-east-1")
			s.Spec.Credentials = nil
			return s
		}(), cloud.ErrWorkloadIdentityForbidden},
	} {
		_, err = awsProber(&probed, nil, egress.ModeStrict).ProbeCloud(ctx, tc.srv, awsConn())
		if !errors.Is(err, tc.want) || errors.Is(err, server.ErrInvalidServer) || len(probed) != 0 {
			t.Errorf("%s: %v", name, err)
		}
	}
	cc := server.CloudConnection{Credentials: awsKeys("AKIASECRETKEY")}
	cc.CABundle = []byte("not a certificate")
	_, err = awsProber(&probed, nil, egress.ModeStrict).ProbeCloud(ctx, awsCluster("us-east-1", ""), cc)
	if !errors.Is(err, s3client.ErrInvalidCA) || len(probed) != 0 || strings.Contains(err.Error(), "AKIASECRETKEY") {
		t.Errorf("CA: %v", err)
	}
}

// A store's CA bundle is the roots of its AWS client; one that holds no
// certificate is refused.
func TestCloudEgressCABundle(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test ca"}, IsCA: true, BasicConstraintsValid: true,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	c := Config{EgressMode: egress.ModeAllowPrivate, Egress: egress.NewPolicy(egress.ModeAllowPrivate)}

	e, err := c.cloudEgress(server.Connection{CABundle: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})})
	if err != nil || e.RootCAs == nil || e.Mode != egress.ModeAllowPrivate {
		t.Errorf("bundle: %+v, %v", e, err)
	}
	if e, err = c.cloudEgress(server.Connection{}); err != nil || e.RootCAs != nil {
		t.Errorf("no bundle: %+v, %v", e, err)
	}
	if _, err = c.cloudEgress(server.Connection{CABundle: []byte("junk")}); !errors.Is(err, s3client.ErrInvalidCA) {
		t.Errorf("junk: %v", err)
	}
}

func TestNewAWSStore(t *testing.T) {
	if _, ok := NewAWSStore(aws.Config{Region: "us-east-1"}).(*awss3.Store); !ok {
		t.Error("NewAWSStore does not build an awss3.Store")
	}
}

// The stamp of an AWS store changes with what the configuration is built from.
func TestAWSStamp(t *testing.T) {
	o := storeObject{uid: "u", generation: 1}
	base := server.CloudConnection{Credentials: awsKeys("a")}
	same := awsStamp(o, server.CloudConnection{Credentials: awsKeys("a")})
	if awsStamp(o, base) != same {
		t.Error("the stamp is not stable")
	}
	other := server.CloudConnection{Credentials: awsKeys("b")}
	withCA := server.CloudConnection{Credentials: awsKeys("a")}
	withCA.CABundle = []byte("ca")
	if awsStamp(o, other) == same || awsStamp(o, withCA) == same || awsStamp(storeObject{uid: "u", generation: 2}, base) == same {
		t.Error("a change does not change the stamp")
	}
}

// With the real Store behind a stub S3, a Bucket with a quota on an AWS store
// is refused with a fixed text, and one without is created.
func TestBucketsOnAWSStore(t *testing.T) {
	var puts int
	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method == http.MethodPut {
			puts++
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(s3.Close)

	h := newAWSHarness(t, func(s *s3v1alpha1.ClusterObjectStore) { s.Spec.Provider.AWS.EndpointOverride = s3.URL })
	// The stub is on loopback: the zero policy blocks nothing, in allow-private.
	h.cfg.Egress, h.cfg.EgressMode, h.cfg.NewAWSStore = egress.Policy{}, egress.ModeAllowPrivate, NewAWSStore
	r := &BucketReconciler{h.cfg}

	quota := h.bucket("capped", func(b *s3v1alpha1.Bucket) {
		b.Spec.Quota = &s3v1alpha1.BucketQuota{HardBytes: resource.MustParse("1Gi")}
	})
	err := h.reconcile(r, quota)
	if !errors.Is(err, awss3.ErrQuotaUnsupported) {
		t.Fatalf("quota: %v", err)
	}
	h.get(quota)
	if quota.GetExternalID() == createIssued {
		t.Error("create-issued was recorded for a claim that cannot be created")
	}
	msg := h.wantCondition(quota.Status.Conditions, conditions.Synced, metav1.ConditionFalse, conditions.ReconcileError)
	if !strings.Contains(msg, "no bucket quota") || strings.Contains(msg, "127.0.0.1") || puts != 0 {
		t.Errorf("message %q, %d writes", msg, puts)
	}

	plain := h.bucket("plain", nil)
	h.reconcileOK(r, plain)
	h.get(plain)
	if st, _, _ := condition(plain.Status.Conditions, conditions.Ready); st != metav1.ConditionTrue || puts == 0 {
		t.Errorf("Ready = %s, %d writes", st, puts)
	}
}
