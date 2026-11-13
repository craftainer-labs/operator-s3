package controller

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
	"github.com/craftainer-labs/lib-operator-go/pkg/conditions"
	"github.com/craftainer-labs/lib-operator-go/pkg/lifecycle"

	s3v1alpha1 "github.com/craftainer-labs/operator-s3/api/v1alpha1"
	"github.com/craftainer-labs/operator-s3/internal/egress"
	"github.com/craftainer-labs/operator-s3/internal/s3client"
)

func TestBucketLifecycleRetain(t *testing.T) {
	h := newHarness(t, common.DeletionRetain, nil)
	r := &BucketReconciler{h.cfg}
	b := h.bucket("uploads", func(b *s3v1alpha1.Bucket) {
		b.Spec.ObjectLock = true
		q := resource.MustParse("50Gi")
		b.Spec.Quota = &s3v1alpha1.BucketQuota{HardBytes: q}
		b.Spec.LifecycleRules = []s3v1alpha1.LifecycleRule{{ID: "tmp", Prefix: "tmp/", ExpirationDays: 7}}
	})
	name := s3client.BucketName(h.ns, "uploads")

	// First reconcile: the deletion policy is pinned, the finalizer added, the
	// bucket created. Object lock implies versioning.
	h.reconcileOK(r, b)
	h.get(b)
	if b.Status.ExternalName != name {
		t.Errorf("externalName = %q, want %q", b.Status.ExternalName, name)
	}
	if b.Status.EffectiveDeletionPolicy != common.DeletionRetain || len(b.Finalizers) != 1 {
		t.Errorf("pin = %q, finalizers = %v", b.Status.EffectiveDeletionPolicy, b.Finalizers)
	}
	h.fake.want(t, fmt.Sprintf("CreateBucket %s lock=true versioning=true quota=%d rules=1", name, int64(50)<<30))
	if st, _, _ := condition(b.Status.Conditions, conditions.Ready); st != metav1.ConditionTrue {
		t.Errorf("Ready = %s", st)
	}
	claim := h.ns + "/uploads/" + string(b.UID)
	if got := h.fake.buckets[name].Marker(); got != claim {
		t.Errorf("marker = %q, want %q", got, claim)
	}

	// The client was built from the store: admin credentials, the default
	// region, the endpoint, TLS 1.2 at least, and the egress policy.
	tg := h.fake.targets[0]
	if tg.Endpoint != "https://rustfs.example.test:9000" || tg.AccessKey != "admin-ak" || tg.SecretKey != "admin-sk" ||
		tg.Region != "us-east-1" || tg.Dial == nil || tg.MinTLS == 0 {
		t.Errorf("target = %+v", tg)
	}

	// Steady state: nothing is written.
	h.fake.reset()
	h.reconcileOK(r, b)
	h.fake.wantNot(t, "CreateBucket", "UpdateBucket", "DeleteBucket")

	// Drift: a changed quota is corrected.
	h.patch(b, func() { b.Spec.Quota = &s3v1alpha1.BucketQuota{HardBytes: resource.MustParse("1Gi")} })
	h.fake.reset()
	h.reconcileOK(r, b)
	h.fake.want(t, fmt.Sprintf("UpdateBucket %s versioning=true quota=%d rules=1", name, int64(1)<<30))

	// A bucket someone else owns is never touched.
	h.fake.buckets[name] = s3client.BucketState{Exists: true, Tags: map[string]string{s3client.MarkerTag: "other/bucket/uid"}}
	h.fake.reset()
	h.reconcileOK(r, b)
	h.get(b)
	h.wantCondition(b.Status.Conditions, conditions.Synced, metav1.ConditionFalse, conditions.NameCollision)
	h.fake.wantNot(t, "CreateBucket", "UpdateBucket", "DeleteBucket")

	// Deleting a Retain bucket leaves it in place and releases the claim.
	h.must(env.Client.Delete(context.Background(), b))
	h.fake.reset()
	h.reconcileOK(r, b)
	if !h.gone(b) {
		t.Error("the claim is still there")
	}
	h.fake.wantNot(t, "DeleteBucket")
	h.rec.AssertEvent(t, "Normal", conditions.EventRetained, "")
}

func TestBucketDeletedWithTheDeletePolicy(t *testing.T) {
	h := newHarness(t, common.DeletionDelete, nil)
	r := &BucketReconciler{h.cfg}
	b := h.bucket("gone", nil)
	h.reconcileOK(r, b)
	h.get(b)
	if b.Status.EffectiveDeletionPolicy != common.DeletionDelete {
		t.Fatalf("pin = %q", b.Status.EffectiveDeletionPolicy)
	}
	name := s3client.BucketName(h.ns, "gone")

	// A bucket that still holds objects stays, with a fixed reason; the claim
	// is kept and retried.
	h.fake.fail["DeleteBucket"] = fmt.Errorf("%w: raw detail", s3client.ErrBucketNotEmpty)
	h.must(env.Client.Delete(context.Background(), b))
	if err := h.reconcile(r, b); !errors.Is(err, s3client.ErrBucketNotEmpty) {
		t.Errorf("not empty: %v", err)
	}
	if h.gone(b) {
		t.Error("the finalizer was dropped over a bucket that holds objects")
	}

	delete(h.fake.fail, "DeleteBucket")
	h.fake.reset()
	h.reconcileOK(r, b)
	h.fake.want(t, "DeleteBucket "+name)
	if !h.gone(b) {
		t.Error("the claim is still there")
	}
}

// A bucket that exists with no marker is never adopted, never changed and
// never deleted, unless the claim's status recorded that it issued the create.
func TestBucketIntentFirstAdoption(t *testing.T) {
	h := newHarness(t, common.DeletionDelete, nil)
	r := &BucketReconciler{h.cfg}
	b := h.bucket("existing", nil)
	name := s3client.BucketName(h.ns, "existing")
	h.fake.buckets[name] = s3client.BucketState{Exists: true, Tags: map[string]string{"team": "a"}}

	h.reconcileOK(r, b)
	h.get(b)
	msg := h.wantCondition(b.Status.Conditions, conditions.Synced, metav1.ConditionFalse, conditions.NameCollision)
	if !strings.Contains(msg, ErrExistsUnmanaged.Error()) {
		t.Errorf("message = %q", msg)
	}
	h.fake.wantNot(t, "CreateBucket", "UpdateBucket", "DeleteBucket")

	// Deleting the claim does not delete the bucket, although the policy is
	// Delete: the finalizer stays.
	h.must(env.Client.Delete(context.Background(), b))
	h.reconcileOK(r, b)
	h.fake.wantNot(t, "DeleteBucket")
	if h.gone(b) {
		t.Error("the finalizer was dropped over a bucket that is not ours")
	}

	// A claim whose status recorded the create but whose bucket has no marker
	// (the operator stopped between the two requests) takes it over.
	b2 := h.bucket("crashed", nil)
	name2 := s3client.BucketName(h.ns, "crashed")
	h.fake.buckets[name2] = s3client.BucketState{Exists: true, Tags: map[string]string{}}
	h.reconcileOK(r, b2) // pins and adds the finalizer; refuses the bucket
	h.get(b2)
	b2.Status.ExternalID = createIssued
	h.must(env.Client.Status().Update(context.Background(), b2))
	h.fake.reset()
	h.reconcileOK(r, b2)
	h.fake.want(t, "UpdateBucket "+name2)
	if got := h.fake.buckets[name2].Marker(); got != h.ns+"/crashed/"+string(b2.UID) {
		t.Errorf("marker = %q", got)
	}
}

// The create is recorded in the status before the request, so a failure leaves
// the record.
func TestBucketRecordsTheCreateFirst(t *testing.T) {
	h := newHarness(t, common.DeletionRetain, nil)
	r := &BucketReconciler{h.cfg}
	b := h.bucket("flaky", nil)
	h.fake.fail["CreateBucket"] = errStore
	if err := h.reconcile(r, b); !errors.Is(err, errStore) {
		t.Errorf("create: %v", err)
	}
	h.get(b)
	if b.Status.ExternalID != createIssued {
		t.Errorf("externalId = %q", b.Status.ExternalID)
	}
	h.wantCondition(b.Status.Conditions, conditions.Synced, metav1.ConditionFalse, conditions.ReconcileError)
	delete(h.fake.fail, "CreateBucket")
	h.reconcileOK(r, b)
	h.fake.want(t, "CreateBucket")

	// A status write that fails stops the create before it is issued.
	b2 := h.bucket("unrecorded", nil)
	wc, err := client.NewWithWatch(env.Config, client.Options{Scheme: env.Scheme})
	h.must(err)
	boom := errors.New("status write failed")
	cfg := h.cfg
	cfg.Client = interceptor.NewClient(wc, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if bk, ok := obj.(*s3v1alpha1.Bucket); ok && bk.Status.ExternalID == createIssued {
				return boom
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	})
	h.fake.reset()
	if err := h.reconcile(&BucketReconciler{cfg}, b2); !errors.Is(err, boom) {
		t.Errorf("record: %v", err)
	}
	h.fake.wantNot(t, "CreateBucket")
}

// A bucket made by somebody else between Observe and the create is a name
// collision, and the record of the create is withdrawn so that the next
// reconcile does not take the bucket over.
func TestBucketCreateRacesWithAnotherCreator(t *testing.T) {
	h := newHarness(t, common.DeletionRetain, nil)
	r := &BucketReconciler{h.cfg}
	b := h.bucket("raced", nil)
	h.fake.fail["CreateBucket"] = s3client.ErrBucketExists
	_ = h.reconcile(r, b)
	h.get(b)
	msg := h.wantCondition(b.Status.Conditions, conditions.Synced, metav1.ConditionFalse, conditions.NameCollision)
	if !strings.Contains(msg, ErrExistsUnmanaged.Error()) {
		t.Errorf("message = %q", msg)
	}
	if b.Status.ExternalID != "" {
		t.Errorf("externalId = %q, want it withdrawn", b.Status.ExternalID)
	}

	// The same, when withdrawing the record fails: the error is the write's.
	b2 := h.bucket("raced-again", nil)
	wc, err := client.NewWithWatch(env.Config, client.Options{Scheme: env.Scheme})
	h.must(err)
	boom := errors.New("status write failed")
	recorded := false
	cfg := h.cfg
	cfg.Client = interceptor.NewClient(wc, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if bk, ok := obj.(*s3v1alpha1.Bucket); ok && bk.Name == "raced-again" {
				if bk.Status.ExternalID == createIssued {
					recorded = true
				} else if recorded && bk.Status.ExternalID == "" {
					return boom
				}
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	})
	if err := h.reconcile(&BucketReconciler{cfg}, b2); !errors.Is(err, boom) {
		t.Errorf("withdraw: %v", err)
	}
}

// An unreachable store is a condition and a retry; the text is the fixed one.
func TestBucketOnAnUnreachableStore(t *testing.T) {
	h := newHarness(t, common.DeletionDelete, nil)
	r := &BucketReconciler{h.cfg}
	b := h.bucket("down", nil)
	h.fake.fail["Bucket"] = fmt.Errorf("%w: connection failed", lifecycle.ErrUnreachable)

	if err := h.reconcile(r, b); !errors.Is(err, lifecycle.ErrUnreachable) {
		t.Errorf("reconcile: %v", err)
	}
	h.get(b)
	msg := h.wantCondition(b.Status.Conditions, conditions.ServerReachable, metav1.ConditionFalse, conditions.ServerUnreachable)
	if !strings.Contains(msg, "connection failed") || strings.Contains(msg, "rustfs.example.test") {
		t.Errorf("message = %q", msg)
	}
	h.must(env.Client.Delete(context.Background(), b))
	if err := h.reconcile(r, b); err == nil {
		t.Error("the delete went through an unreachable store")
	}
	if h.gone(b) {
		t.Error("the finalizer was dropped")
	}

	// Back up: the claim is created and deleted.
	delete(h.fake.fail, "Bucket")
	h.reconcileOK(r, b)
	if !h.gone(b) {
		t.Error("the claim is still there")
	}
}

func TestBucketWithoutAStoreOrANamespace(t *testing.T) {
	h := newHarness(t, common.DeletionRetain, func(s *s3v1alpha1.ClusterObjectStore) {
		s.Spec.AllowedNamespaces = common.AllowedNamespaces{
			From: common.NamespacesSelector, Selector: &common.LabelSelector{MatchLabels: map[string]string{"allowed": "yes"}},
		}
	})
	r := &BucketReconciler{h.cfg}

	b := h.bucket("outside", nil)
	h.reconcileOK(r, b)
	h.get(b)
	h.wantCondition(b.Status.Conditions, conditions.Synced, metav1.ConditionFalse, conditions.NamespaceNotAllowed)

	missing := h.bucket("nowhere", func(b *s3v1alpha1.Bucket) { b.Spec.ServerRef.Name = "no-such-store" })
	h.reconcileOK(r, missing)
	h.get(missing)
	h.wantCondition(missing.Status.Conditions, conditions.Synced, metav1.ConditionFalse, conditions.ServerNotFound)
	if len(h.fake.targets) != 0 {
		t.Errorf("built %d clients", len(h.fake.targets))
	}

	// A claim that no longer exists is not an error.
	h.reconcileOK(r, &s3v1alpha1.Bucket{ObjectMeta: metav1.ObjectMeta{Name: "never-created", Namespace: h.ns}})
}

// Store settings that cannot be used are reported without a request: a proxy
// with credentials in its URL, admin credentials missing, an endpoint the
// client refuses.
func TestBucketOnAnInvalidStore(t *testing.T) {
	h := newHarness(t, common.DeletionRetain, nil)
	r := &BucketReconciler{h.cfg}
	b := h.bucket("invalid", nil)

	// User decision 1: the proxy URL may not carry credentials, and the
	// operator refuses to connect through one even if admission let it in.
	h.get(h.server)
	patch := client.MergeFrom(h.server.DeepCopy())
	h.server.Spec.ProxyURL = "http://proxy-user:s3cret-proxy-pw@proxy.example.test:3128"
	h.must(env.Client.Patch(context.Background(), h.server, patch))
	err := h.reconcile(r, b)
	if err == nil || !strings.Contains(err.Error(), egress.ErrProxyUserinfo.Error()) {
		t.Fatalf("proxy with userinfo: %v", err)
	}
	h.get(b)
	msg := h.wantCondition(b.Status.Conditions, conditions.Synced, metav1.ConditionFalse, conditions.ServerUnreachable)
	for _, leak := range []string{"s3cret-proxy-pw", "proxy-user"} {
		if strings.Contains(msg, leak) || strings.Contains(err.Error(), leak) {
			t.Errorf("the proxy credentials %q leaked: %s", leak, msg)
		}
		h.rec.AssertNoSecret(t, leak)
	}
	if len(h.fake.targets) != 0 {
		t.Error("connected through a proxy with credentials")
	}

	// An admin Secret without a secret key.
	patch = client.MergeFrom(h.server.DeepCopy())
	h.server.Spec.ProxyURL = ""
	h.must(env.Client.Patch(context.Background(), h.server, patch))
	h.dropAdminKey(AdminSecretKey)
	if err := h.reconcile(r, b); !errors.Is(err, ErrAdminCredentials) {
		t.Errorf("no secret key: %v", err)
	}

	// An endpoint the client refuses.
	h.restoreAdmin()
	cfg := h.cfg
	cfg.NewStore = func(s3client.Target) (Store, error) { return nil, s3client.ErrInvalidEndpoint }
	if err := h.reconcile(&BucketReconciler{cfg}, b); !errors.Is(err, s3client.ErrInvalidEndpoint) {
		t.Errorf("bad endpoint: %v", err)
	}
}

func (h *harness) dropAdminKey(key string) {
	h.t.Helper()
	var admin corev1.Secret
	h.must(env.Client.Get(context.Background(), client.ObjectKey{Namespace: h.ops, Name: h.server.Spec.AdminCredentialsSecretRef.Name}, &admin))
	delete(admin.Data, key)
	h.must(env.Client.Update(context.Background(), &admin))
}

func (h *harness) restoreAdmin() {
	h.t.Helper()
	var admin corev1.Secret
	h.must(env.Client.Get(context.Background(), client.ObjectKey{Namespace: h.ops, Name: h.server.Spec.AdminCredentialsSecretRef.Name}, &admin))
	admin.Data = map[string][]byte{"accessKey": []byte("admin-ak"), "secretKey": []byte("admin-sk")}
	h.must(env.Client.Update(context.Background(), &admin))
}

// Failures reading the cluster surface as errors, not as a silent skip.
func TestBucketReadFailures(t *testing.T) {
	h := newHarness(t, common.DeletionRetain, nil)
	b := h.bucket("reads", nil)
	boom := errors.New("api server down")

	// The second read of the store (after Resolve) fails.
	gets := 0
	cfg := h.cfg
	wc, err := client.NewWithWatch(env.Config, client.Options{Scheme: env.Scheme})
	h.must(err)
	cfg.Client = interceptor.NewClient(wc, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*s3v1alpha1.ClusterObjectStore); ok {
				if gets++; gets == 2 {
					return boom
				}
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	if err := h.reconcile(&BucketReconciler{cfg}, b); !errors.Is(err, boom) {
		t.Errorf("store read: %v", err)
	}
}

// fetchStore reads a namespaced store from the claim's namespace and a cluster
// store by name.
func TestStoreSpecOfBothKinds(t *testing.T) {
	h := newHarness(t, common.DeletionRetain, nil)
	ns := &s3v1alpha1.ObjectStore{ObjectMeta: metav1.ObjectMeta{Name: "local", Namespace: h.ns}}
	ns.Spec.Endpoint = "https://local.example.test:9000"
	ns.Spec.Region = "eu-west-1"
	ns.Spec.AdminCredentialsSecretRef = common.AdminSecretRef{Name: "local-admin"}
	h.must(env.Client.Create(context.Background(), ns))

	got, err := h.cfg.fetchStore(context.Background(), h.ns, common.ServerRef{Kind: s3v1alpha1.KindObjectStore, Name: "local"})
	if err != nil || got.spec.Region != "eu-west-1" || got.uid != ns.UID || got.uid == "" {
		t.Errorf("namespaced: %+v, %v", got, err)
	}
	got, err = h.cfg.fetchStore(context.Background(), h.ns, h.ref())
	if err != nil || got.spec.Endpoint != "https://rustfs.example.test:9000" {
		t.Errorf("cluster: %+v, %v", got, err)
	}
	if _, err = h.cfg.fetchStore(context.Background(), h.ns, common.ServerRef{Kind: s3v1alpha1.KindObjectStore, Name: "absent"}); err == nil {
		t.Error("a missing namespaced store was found")
	}
}

func TestBackendRegionAndClientStore(t *testing.T) {
	if (&backend{}).region() != "us-east-1" || (&backend{spec: s3v1alpha1.ObjectStoreConfig{Region: "eu-west-1"}}).region() != "eu-west-1" {
		t.Error("region")
	}
	if _, err := NewClientStore(s3client.Target{Endpoint: "http://insecure"}); !errors.Is(err, s3client.ErrInvalidEndpoint) {
		t.Errorf("bad target: %v", err)
	}
	dial := func(context.Context, string, string) (net.Conn, error) { return nil, errStore }
	st, err := NewClientStore(s3client.Target{Endpoint: "https://store.example.test", AccessKey: "a", SecretKey: "s", Dial: dial})
	if err != nil || st == nil {
		t.Errorf("good target: %v, %v", st, err)
	}
}

// The spec maps to what the store is asked for; the checks that the schema
// makes are made again here.
func TestBucketParams(t *testing.T) {
	e := &bucketExternal{name: "b", marker: "m"}
	p, err := e.params(s3v1alpha1.BucketSpec{Versioning: true})
	if err != nil || !p.Versioning || p.ObjectLock || p.Quota != 0 || len(p.Rules) != 0 {
		t.Errorf("params = %+v, %v", p, err)
	}
	p, _ = e.params(s3v1alpha1.BucketSpec{ObjectLock: true})
	if !p.Versioning || !p.ObjectLock {
		t.Errorf("object lock must imply versioning: %+v", p)
	}
	dup := s3v1alpha1.BucketSpec{LifecycleRules: []s3v1alpha1.LifecycleRule{{ID: "a", ExpirationDays: 1}, {ID: "a", ExpirationDays: 2}}}
	if _, err := e.params(dup); !errors.Is(err, ErrInvalidLifecycle) {
		t.Errorf("duplicate ids: %v", err)
	}
	// Observe, Create and Update all refuse it, and Create records nothing.
	be := &backend{store: newMemStore()}
	e = &bucketExternal{be: be, claim: &s3v1alpha1.Bucket{}, name: "b", marker: "m"}
	e.persist = func(context.Context) error { t.Error("Create recorded an intent for an invalid spec"); return nil }
	if _, err := e.Create(context.Background(), dup); !errors.Is(err, ErrInvalidLifecycle) {
		t.Errorf("create: %v", err)
	}
	if _, err := e.Update(context.Background(), dup, s3client.BucketState{}); !errors.Is(err, ErrInvalidLifecycle) {
		t.Errorf("update: %v", err)
	}
	be.store.(*memStore).buckets["b"] = s3client.BucketState{Exists: true, Tags: map[string]string{s3client.MarkerTag: "m"}}
	if _, err := e.Observe(context.Background(), dup); !errors.Is(err, ErrInvalidLifecycle) {
		t.Errorf("observe: %v", err)
	}
}

func TestNewBucketExternalKeepsTheRecordedName(t *testing.T) {
	claim := &s3v1alpha1.Bucket{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "tenant"}}
	e := newBucketExternal(&backend{}, claim)
	if e.name != s3client.BucketName("tenant", "b") || claim.GetExternalName() != e.name {
		t.Errorf("name = %q", e.name)
	}
	claim.SetExternalName("kept")
	if e = newBucketExternal(&backend{}, claim); e.name != "kept" {
		t.Errorf("name = %q", e.name)
	}
}
