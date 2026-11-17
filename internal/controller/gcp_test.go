package controller

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
	"github.com/craftainer-labs/lib-operator-go/pkg/cloud"
	"github.com/craftainer-labs/lib-operator-go/pkg/conditions"
	"github.com/craftainer-labs/lib-operator-go/pkg/egress"
	"github.com/craftainer-labs/lib-operator-go/pkg/server"

	s3v1alpha1 "github.com/craftainer-labs/operator-s3/api/v1alpha1"
	"github.com/craftainer-labs/operator-s3/internal/gcs"
	"github.com/craftainer-labs/operator-s3/internal/s3client"
)

const (
	gcpKeyFile = "gcp-creds"
	gcpProject = "proj-12345"
)

var (
	gcpKeyOnce sync.Once
	gcpKeyPEM  string
)

// gcpKeys is the credentials Secret of a GCP store: a service_account key with
// a freshly generated private key (once per test binary). The token endpoint
// is never called: tokens are fetched on first use.
func gcpKeys(t *testing.T, clientEmail string) map[string][]byte {
	t.Helper()
	gcpKeyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		der, err := x509.MarshalPKCS8PrivateKey(k)
		if err != nil {
			t.Fatal(err)
		}
		gcpKeyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	})
	// Assigned key by key: gosec G101 takes a literal holding "private_key" for a
	// hardcoded credential, and this one is generated above.
	doc := map[string]string{}
	doc["type"], doc["project_id"], doc["private_key_id"] = "service_account", gcpProject, "kid"
	doc["private_key"], doc["client_email"], doc["client_id"] = gcpKeyPEM, clientEmail, "1"
	doc["token_uri"] = "https://oauth2.example.test/token"
	key, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return map[string][]byte{cloud.KeyGCPCredentials: key}
}

// gcpCall is what a GCP store was built from.
type gcpCall struct {
	client            *cloud.GCPClient
	project, location string
}

// gcpRecorder records the stores and probes built for GCP stores.
type gcpRecorder struct {
	stores []gcpCall
	probed []gcpCall
	err    error
}

func (r *gcpRecorder) newStore(store Store) NewGCPStoreFunc {
	return func(_ context.Context, c *cloud.GCPClient, project, location string) (Store, error) {
		r.stores = append(r.stores, gcpCall{c, project, location})
		return store, r.err
	}
}

func (r *gcpRecorder) probe(_ context.Context, c *cloud.GCPClient, project string) error {
	r.probed = append(r.probed, gcpCall{c, project, ""})
	return r.err
}

// newGCPHarness is a harness whose cluster store runs on GCP in europe-west1
// with the credentials of a Secret in the operator namespace.
func newGCPHarness(t *testing.T, mutate func(*s3v1alpha1.ClusterObjectStore)) (*harness, *gcpRecorder) {
	t.Helper()
	h := newHarnessOps(t, common.DeletionRetain, func(ops string, s *s3v1alpha1.ClusterObjectStore) {
		s.Spec.Endpoint, s.Spec.AdminCredentialsSecretRef = "", nil
		s.Spec.Provider = &s3v1alpha1.ClusterStoreProvider{
			Type: cloud.ProviderGCP,
			GCP:  &cloud.GCPClusterBlock{GCPBlock: cloud.GCPBlock{Project: gcpProject, Location: "europe-west1"}},
		}
		s.Spec.Credentials = &cloud.Credentials{Source: cloud.SourceSecret, SecretRef: &cloud.SecretRef{Name: gcpKeyFile, Namespace: ops}}
		if mutate != nil {
			mutate(s)
		}
	})
	h.must(env.Client.Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: gcpKeyFile, Namespace: h.ops}, Data: gcpKeys(t, "operator@proj-12345.iam.gserviceaccount.com"),
	}))
	h.cfg.Cache = NewStoreCache(time.Hour)
	rec := &gcpRecorder{}
	h.cfg.NewGCPStore = rec.newStore(h.fake)
	return h, rec
}

// The Store of a store on GCP is built from the credentials Secret, the
// project and the location, and kept until the Secret changes.
func TestBackendBuildsTheGCPStore(t *testing.T) {
	h, rec := newGCPHarness(t, nil)
	ctx := context.Background()

	b, err := h.cfg.backend(ctx, h.ns, h.ref())
	if err != nil || b.store != Store(h.fake) || len(rec.stores) != 1 {
		t.Fatalf("%v, %v, %d stores", b, err, len(rec.stores))
	}
	if c := rec.stores[0]; c.project != gcpProject || c.location != "europe-west1" || c.client.Endpoint != "" || c.client.HTTPClient == nil {
		t.Errorf("store call = %+v", c)
	}
	if b.host != "storage.googleapis.com" || b.conn.Endpoint != "https://storage.googleapis.com" || b.gen != nil || b.keys != nil {
		t.Errorf("host %q, endpoint %q", b.host, b.conn.Endpoint)
	}

	// Nothing changed: the Store is reused.
	if _, err = h.cfg.backend(ctx, h.ns, h.ref()); err != nil || len(rec.stores) != 1 {
		t.Errorf("reuse: %v, %d stores", err, len(rec.stores))
	}
	// A new key in the Secret: a new Store.
	h.must(env.Client.Update(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: gcpKeyFile, Namespace: h.ops}, Data: gcpKeys(t, "rotated@proj-12345.iam.gserviceaccount.com"),
	}))
	if _, err = h.cfg.backend(ctx, h.ns, h.ref()); err != nil || len(rec.stores) != 2 {
		t.Errorf("rotated: %v, %d stores", err, len(rec.stores))
	}
}

// A namespaced store has its own credentials Secret and no override.
func TestBackendBuildsTheGCPStoreOfANamespacedStore(t *testing.T) {
	h, rec := newGCPHarness(t, nil)
	ctx := context.Background()
	h.must(env.Client.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: gcpKeyFile, Namespace: h.ns}, Data: gcpKeys(t, "tenant@proj-12345.iam.gserviceaccount.com"),
	}))
	st := &s3v1alpha1.ObjectStore{ObjectMeta: metav1.ObjectMeta{Name: "mine", Namespace: h.ns}}
	st.Spec.Provider = &s3v1alpha1.NamespacedStoreProvider{Type: cloud.ProviderGCP, GCP: &cloud.GCPBlock{Project: "tenant-12345"}}
	st.Spec.Credentials = &cloud.NamespacedCredentials{Source: cloud.SourceSecret, SecretRef: &cloud.SecretRef{Name: gcpKeyFile}}
	h.must(env.Client.Create(ctx, st))

	b, err := h.cfg.backend(ctx, h.ns, common.ServerRef{Kind: s3v1alpha1.KindObjectStore, Name: "mine"})
	if err != nil || len(rec.stores) != 1 || b == nil {
		t.Fatalf("%v, %v", b, err)
	}
	if c := rec.stores[0]; c.project != "tenant-12345" || c.location != "" {
		t.Errorf("store call = %+v", c)
	}
}

// The endpoint override is for tests of the operator with EGRESS_MODE=allow-private.
func TestGCPBackendEndpointOverride(t *testing.T) {
	ctx := context.Background()
	override := func(url string) func(*s3v1alpha1.ClusterObjectStore) {
		return func(s *s3v1alpha1.ClusterObjectStore) { s.Spec.Provider.GCP.EndpointOverride = url }
	}

	// Strict: refused, before anything is built.
	h, rec := newGCPHarness(t, override("http://10.1.2.3:4443"))
	_, err := h.cfg.backend(ctx, h.ns, h.ref())
	if !errors.Is(err, server.ErrInvalidServer) || !errors.Is(err, cloud.ErrEndpointOverrideForbidden) || len(rec.stores) != 0 {
		t.Errorf("strict: %v", err)
	}

	// Allow-private: honoured, and the host is the override's.
	h, rec = newGCPHarness(t, override("http://10.1.2.3:4443"))
	h.cfg.EgressMode = egress.ModeAllowPrivate
	b, err := h.cfg.backend(ctx, h.ns, h.ref())
	if err != nil || b.host != "10.1.2.3:4443" || len(rec.stores) != 1 || rec.stores[0].client.Endpoint != "http://10.1.2.3:4443" {
		t.Errorf("allow-private: %+v, %v", b, err)
	}

	// An override that is no URL passes the schema's pattern and is refused
	// when the client is built.
	h, _ = newGCPHarness(t, override("http://[::1"))
	h.cfg.EgressMode = egress.ModeAllowPrivate
	if b, err = h.cfg.backend(ctx, h.ns, h.ref()); !errors.Is(err, cloud.ErrInvalidEndpoint) || b != nil {
		t.Errorf("bad override: %+v, %v", b, err)
	}
}

// A store whose credentials cannot be had says so without the key, and one
// whose client cannot be built is an invalid server.
func TestGCPBackendFailures(t *testing.T) {
	ctx := context.Background()

	// The Secret holds a key of another type.
	h, rec := newGCPHarness(t, nil)
	h.must(env.Client.Update(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: gcpKeyFile, Namespace: h.ops},
		Data:       map[string][]byte{cloud.KeyGCPCredentials: []byte(`{"type":"external_account","secret":"GCPSECRETVALUE"}`)},
	}))
	_, err := h.cfg.backend(ctx, h.ns, h.ref())
	if !errors.Is(err, server.ErrInvalidServer) || !errors.Is(err, cloud.ErrCredentialsUnavailable) || strings.Contains(err.Error(), "GCPSECRETVALUE") {
		t.Errorf("external_account: %v", err)
	}
	if h.cfg.Cache.Len() != 0 || len(rec.stores) != 0 {
		t.Error("a failed build was cached")
	}

	// A CA bundle that holds no certificate.
	cc := server.CloudConnection{Credentials: gcpKeys(t, "op@proj-12345.iam.gserviceaccount.com")}
	cc.CABundle = []byte("junk")
	cs := cloudStore{set: true, kind: cloud.ProviderGCP, scope: cloud.ScopeCluster, creds: &cloud.Credentials{Source: cloud.SourceSecret}}
	if _, err = h.cfg.gcpClient(ctx, cs, cc); !errors.Is(err, s3client.ErrInvalidCA) {
		t.Errorf("CA: %v", err)
	}

	// The library cannot build the store.
	h, rec = newGCPHarness(t, nil)
	rec.err = errors.New("no client")
	if _, err = h.cfg.backend(ctx, h.ns, h.ref()); !errors.Is(err, server.ErrInvalidServer) || len(rec.stores) != 1 {
		t.Errorf("store: %v", err)
	}
}

// The production constructor builds the store, and says when it cannot.
func TestNewGCPStore(t *testing.T) {
	c := &cloud.GCPClient{HTTPClient: http.DefaultClient}
	s, err := NewGCPStore(context.Background(), c, gcpProject, "eu")
	if _, ok := s.(*gcs.Store); err != nil || !ok {
		t.Errorf("%T, %v", s, err)
	}
	t.Setenv("STORAGE_EMULATOR_HOST", "http://[::1")
	if s, err = NewGCPStore(context.Background(), c, gcpProject, "eu"); err == nil || s != nil {
		t.Errorf("an unusable client was built: %v, %v", s, err)
	}
}

func gcpCluster(location, override string) *s3v1alpha1.ClusterObjectStore {
	s := &s3v1alpha1.ClusterObjectStore{}
	s.Spec.Provider = &s3v1alpha1.ClusterStoreProvider{
		Type: cloud.ProviderGCP,
		GCP:  &cloud.GCPClusterBlock{GCPBlock: cloud.GCPBlock{Project: gcpProject, Location: location}, EndpointOverride: override},
	}
	s.Spec.Credentials = &cloud.Credentials{Source: cloud.SourceSecret}
	return s
}

func gcpNamespaced() *s3v1alpha1.ObjectStore {
	s := &s3v1alpha1.ObjectStore{}
	s.Spec.Provider = &s3v1alpha1.NamespacedStoreProvider{Type: cloud.ProviderGCP, GCP: &cloud.GCPBlock{Project: gcpProject}}
	s.Spec.Credentials = &cloud.NamespacedCredentials{Source: cloud.SourceSecret}
	return s
}

func gcpProber(rec *gcpRecorder, mode egress.Mode) Prober {
	return Prober{Config: Config{Egress: egress.NewPolicy(mode), EgressMode: mode, ProbeGCP: rec.probe}}
}

// The project, the region (when the store names one) and the endpoint (scheme
// and host only) are discovered, and the version stays empty.
func TestProberDiscoversTheProjectAndEndpoint(t *testing.T) {
	ctx := context.Background()
	cc := server.CloudConnection{Credentials: gcpKeys(t, "probe@proj-12345.iam.gserviceaccount.com")}
	for name, tc := range map[string]struct {
		srv              server.CloudServer
		mode             egress.Mode
		region, endpoint string
	}{
		"cluster":   {gcpCluster("us-east1", ""), egress.ModeStrict, "us-east1", "https://storage.googleapis.com"},
		"no region": {gcpCluster("", ""), egress.ModeStrict, "", "https://storage.googleapis.com"},
		"emulator":  {gcpCluster("", "http://10.1.2.3:4443"), egress.ModeAllowPrivate, "", "http://10.1.2.3:4443"},
		"namespace": {gcpNamespaced(), egress.ModeStrict, "", "https://storage.googleapis.com"},
	} {
		rec := &gcpRecorder{}
		res, err := gcpProber(rec, tc.mode).ProbeCloud(ctx, tc.srv, cc)
		want := 2
		if tc.region != "" {
			want = 3
		}
		if err != nil || res.Version != "" || len(res.Discovered) != want || res.Discovered[DiscoveredRegion] != tc.region ||
			res.Discovered[DiscoveredEndpoint] != tc.endpoint || res.Discovered[DiscoveredAccountID] != gcpProject {
			t.Errorf("%s: %+v, %v", name, res, err)
		}
		if len(rec.probed) != 1 || rec.probed[0].project != gcpProject {
			t.Errorf("%s: probed %+v", name, rec.probed)
		}
	}
}

// A failed probe is shown as unreachable, never as an invalid server, and
// carries no credentials.
func TestGCPProbeFailures(t *testing.T) {
	ctx := context.Background()
	cc := server.CloudConnection{Credentials: gcpKeys(t, "probe@proj-12345.iam.gserviceaccount.com")}

	rec := &gcpRecorder{err: errors.Join(s3client.ErrConnAuth, errors.New("denied"))}
	res, err := gcpProber(rec, egress.ModeStrict).ProbeCloud(ctx, gcpCluster("", ""), cc)
	if !errors.Is(err, s3client.ErrConnAuth) || errors.Is(err, server.ErrInvalidServer) || res.Discovered != nil {
		t.Errorf("probe: %+v, %v", res, err)
	}

	// An override in strict mode, and a namespaced store on the operator's
	// identity, are refused before anything is asked of Google.
	rec = &gcpRecorder{}
	for name, tc := range map[string]struct {
		srv  server.CloudServer
		want error
	}{
		"override": {gcpCluster("", "http://10.1.2.3:4443"), cloud.ErrEndpointOverrideForbidden},
		"identity": {func() server.CloudServer {
			s := gcpNamespaced()
			s.Spec.Credentials = nil
			return s
		}(), cloud.ErrWorkloadIdentityForbidden},
	} {
		_, err = gcpProber(rec, egress.ModeStrict).ProbeCloud(ctx, tc.srv, cc)
		if !errors.Is(err, tc.want) || errors.Is(err, server.ErrInvalidServer) || len(rec.probed) != 0 {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// encodingStore is a store that, like a GCS one, holds the marker in a form of
// its own.
type encodingStore struct{ *memStore }

func (encodingStore) EncodeMarker(marker string) string { return "enc:" + marker }

// A Bucket on a store that encodes the marker is created with the encoded form
// and found up to date with it.
func TestBucketsOnAStoreThatEncodesTheMarker(t *testing.T) {
	h, _ := newGCPHarness(t, nil)
	h.cfg.NewGCPStore = func(context.Context, *cloud.GCPClient, string, string) (Store, error) {
		return encodingStore{h.fake}, nil
	}
	r := &BucketReconciler{h.cfg}

	b := h.bucket("plain", nil)
	h.reconcileOK(r, b)
	h.get(b)
	if st, _, _ := condition(b.Status.Conditions, conditions.Ready); st != metav1.ConditionTrue {
		t.Fatalf("Ready = %s", st)
	}
	name := s3client.BucketName(h.ns, "plain")
	if got := h.fake.buckets[name].Tags[s3client.MarkerTag]; !strings.HasPrefix(got, "enc:"+h.ns+"/plain/") {
		t.Errorf("marker = %q", got)
	}
	h.fake.reset()
	h.reconcileOK(r, b)
	h.fake.wantNot(t, "CreateBucket", "UpdateBucket")
}

// A bucket of the claim that the store reports Drifted is not up to date: the
// store is asked to update it.
func TestADriftedBucketIsUpdated(t *testing.T) {
	h, _ := newGCPHarness(t, nil)
	r := &BucketReconciler{h.cfg}
	b := h.bucket("hardening", nil)
	h.reconcileOK(r, b)
	name := s3client.BucketName(h.ns, "hardening")
	h.fake.reset()
	h.reconcileOK(r, b)
	h.fake.wantNot(t, "CreateBucket", "UpdateBucket")

	st := h.fake.buckets[name]
	st.Drifted = true
	h.fake.buckets[name] = st
	h.fake.reset()
	h.reconcileOK(r, b)
	h.fake.want(t, "UpdateBucket "+name)
}

// atomicStore is a Store that writes the marker in the request that creates the
// bucket (as the GCS store does).
type atomicStore struct{ *memStore }

func (atomicStore) MarkerAtCreate() {}

// On a store that writes the marker at create, a bucket without one is never the
// claim's, whatever the status records: it is refused and never updated.
func TestAnUnmarkedBucketIsNeverTakenOverOnAnAtomicMarkerStore(t *testing.T) {
	h, _ := newGCPHarness(t, nil)
	h.cfg.NewGCPStore = func(context.Context, *cloud.GCPClient, string, string) (Store, error) {
		return atomicStore{h.fake}, nil
	}
	r := &BucketReconciler{h.cfg}

	b := h.bucket("crashed", nil)
	name := s3client.BucketName(h.ns, "crashed")
	h.fake.buckets[name] = s3client.BucketState{Exists: true, Tags: map[string]string{}}
	h.reconcileOK(r, b) // pins and adds the finalizer; refuses the bucket
	h.get(b)
	b.Status.ExternalID = createIssued
	h.must(env.Client.Status().Update(context.Background(), b))
	h.fake.reset()
	h.reconcileOK(r, b)
	h.fake.wantNot(t, "CreateBucket", "UpdateBucket", "DeleteBucket")
	if got := h.fake.buckets[name].Marker(); got != "" {
		t.Errorf("marker = %q", got)
	}
}
