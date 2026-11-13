package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
	"github.com/craftainer-labs/lib-operator-go/pkg/conditions"
	"github.com/craftainer-labs/lib-operator-go/pkg/lifecycle"

	s3v1alpha1 "github.com/craftainer-labs/operator-s3/api/v1alpha1"
	"github.com/craftainer-labs/operator-s3/internal/s3client"
)

func bindingSecret(h *harness, a *s3v1alpha1.BucketAccess) map[string]string {
	h.t.Helper()
	var s corev1.Secret
	h.must(env.Client.Get(context.Background(), client.ObjectKey{Namespace: h.ns, Name: a.GetBindingSecretName()}, &s))
	if s.Type != "servicebinding.io/s3" {
		h.t.Errorf("Secret type = %q", s.Type)
	}
	out := map[string]string{}
	for k, v := range s.Data {
		out[k] = string(v)
	}
	return out
}

func TestAccessLifecycleAndRotation(t *testing.T) {
	h := newHarness(t, common.DeletionRetain, nil)
	br, ar := &BucketReconciler{h.cfg}, &AccessReconciler{h.cfg}
	b := h.bucket("uploads", nil)
	h.reconcileOK(br, b)
	bucket := s3client.BucketName(h.ns, "uploads")

	grace := metav1.Duration{Duration: time.Hour}
	interval := metav1.Duration{Duration: 24 * time.Hour}
	a := h.access("uploads-app", "uploads", func(a *s3v1alpha1.BucketAccess) {
		a.Spec.Access = s3v1alpha1.AccessReadOnly
		a.Spec.Prefix = "app/"
		a.Spec.RotationPolicy = &common.RotationPolicy{Interval: &interval, GracePeriod: &grace}
	})
	user := s3client.UserName(h.ns, "uploads-app")

	// Create: the user with the bucket policy, and the first generation's key
	// (derived from the claim's UID), in the binding Secret.
	h.fake.reset()
	h.reconcileOK(ar, a)
	h.get(a)
	key0 := s3client.AccessKey(string(a.UID), 0)
	if a.Status.ExternalName != user || a.Status.ExternalID != key0 || a.Status.Binding == nil || a.Status.Binding.Name != "uploads-app" {
		t.Errorf("status = %+v", a.Status)
	}
	h.fake.want(t, "ApplyAccess "+user+" "+user+"-", "IssueKey "+user+" "+key0)
	data := bindingSecret(h, a)
	if data["accessKeyId"] != key0 || len(data["secretAccessKey"]) != 40 || data["bucket"] != bucket || data["prefix"] != "app/" ||
		data["endpoint"] != "https://rustfs.example.test:9000" || data["region"] != "us-east-1" || data["pathStyle"] != "true" ||
		data["type"] != "s3" || data["provider"] != "craftainer.dev" {
		t.Errorf("binding = %v", data)
	}
	firstSecret := data["secretAccessKey"]
	if got := h.fake.docs[h.fake.users[user].policies[0]]; !strings.Contains(got, "arn:aws:s3:::"+bucket+"/app/*") || strings.Contains(got, "PutObject") {
		t.Errorf("policy = %s", got)
	}
	// The secret key is in the Secret only: not in an Event or a condition.
	h.rec.AssertNoSecret(t, firstSecret, h.fake.keys[key0])
	for _, c := range a.Status.Conditions {
		if strings.Contains(c.Message, firstSecret) {
			t.Errorf("the secret key is in condition %s", c.Type)
		}
	}

	// Steady state: nothing is written and the Secret is kept.
	h.fake.reset()
	h.reconcileOK(ar, a)
	h.fake.wantNot(t, "ApplyAccess", "IssueKey", "DeleteKey", "DeleteAccess")
	if bindingSecret(h, a)["secretAccessKey"] != firstSecret {
		t.Error("the secret key changed without a reason")
	}

	// Rotation: the annotation issues generation 1; generation 0 stays valid.
	h.patch(a, func() { a.Annotations = map[string]string{lifecycle.AnnotationRotate: "1"} })
	h.fake.reset()
	h.reconcileOK(ar, a)
	key1 := s3client.AccessKey(string(a.UID), 1)
	h.fake.want(t, "IssueKey "+user+" "+key1)
	h.fake.wantNot(t, "DeleteKey")
	data = bindingSecret(h, a)
	if data["accessKeyId"] != key1 || data["secretAccessKey"] == firstSecret {
		t.Errorf("rotated binding = %v", data)
	}
	if _, ok := h.fake.keys[key0]; !ok {
		t.Error("the previous key was retired during the grace period")
	}
	h.get(a)
	if a.Status.Rotation == nil || a.Status.Rotation.Active != key1 || a.Status.Rotation.Sequence != 1 || a.Status.ExternalID != key1 {
		t.Fatalf("rotation = %+v, externalId = %q", a.Status.Rotation, a.Status.ExternalID)
	}

	// After the grace period the previous key is retired.
	h.now = h.now.Add(2 * time.Hour)
	h.fake.reset()
	h.reconcileOK(ar, a)
	h.fake.want(t, "DeleteKey "+key0)
	if _, ok := h.fake.keys[key0]; ok {
		t.Error("the previous key was not retired")
	}
	h.rec.AssertEvent(t, "Normal", conditions.EventRotationFinished, "")

	// Drift: the access level changed; the new policy is applied and the active
	// key's secret is set again.
	h.patch(a, func() { a.Spec.Access = s3v1alpha1.AccessReadWrite })
	h.fake.reset()
	h.reconcileOK(ar, a)
	h.fake.want(t, "ApplyAccess "+user, "IssueKey "+user+" "+key1)
	if got := h.fake.docs[h.fake.users[user].policies[0]]; !strings.Contains(got, "PutObject") {
		t.Errorf("policy = %s", got)
	}
	if bindingSecret(h, a)["accessKeyId"] != key1 {
		t.Error("the Secret does not hold the active key")
	}

	// A key that was deleted behind our back is issued again.
	delete(h.fake.keys, key1)
	h.fake.reset()
	h.reconcileOK(ar, a)
	h.fake.want(t, "IssueKey "+user+" "+key1)

	// Deleting removes the user, its keys and its policies.
	h.must(env.Client.Delete(context.Background(), a))
	h.fake.reset()
	h.reconcileOK(ar, a)
	h.fake.want(t, "DeleteAccess "+user+" "+user+"-")
	if !h.gone(a) {
		t.Error("the claim is still there")
	}
	if len(h.fake.users) != 0 {
		t.Errorf("users = %v", h.fake.users)
	}
}

// An IAM user that exists while the claim recorded no create is not ours: it
// is never adopted, changed or deleted.
func TestAccessNeverAdoptsAnUnmarkedUser(t *testing.T) {
	h := newHarness(t, common.DeletionRetain, nil)
	br, ar := &BucketReconciler{h.cfg}, &AccessReconciler{h.cfg}
	h.reconcileOK(br, h.bucket("uploads", nil))
	user := s3client.UserName(h.ns, "app")
	h.fake.users[user] = &memUser{enabled: true, policies: []string{"readwrite"}}
	a := h.access("app", "uploads", nil)

	h.reconcileOK(ar, a)
	h.get(a)
	msg := h.wantCondition(a.Status.Conditions, conditions.Synced, metav1.ConditionFalse, conditions.NameCollision)
	if !strings.Contains(msg, ErrExistsUnmanaged.Error()) {
		t.Errorf("message = %q", msg)
	}
	h.fake.wantNot(t, "ApplyAccess", "IssueKey", "DeleteAccess")

	h.must(env.Client.Delete(context.Background(), a))
	h.reconcileOK(ar, a)
	h.fake.wantNot(t, "DeleteAccess")
	if h.gone(a) {
		t.Error("the finalizer was dropped over a user that is not ours")
	}
}

func TestAccessNeedsAUsableBucket(t *testing.T) {
	h := newHarness(t, common.DeletionRetain, nil)
	br, ar := &BucketReconciler{h.cfg}, &AccessReconciler{h.cfg}

	// No such Bucket: nothing is created.
	a := h.access("no-bucket", "absent", nil)
	if err := h.reconcile(ar, a); !errors.Is(err, ErrBucketNotUsable) {
		t.Errorf("missing bucket: %v", err)
	}
	h.fake.wantNot(t, "ApplyAccess")

	// A Bucket on another store is not usable either.
	other := h.bucket("elsewhere", func(b *s3v1alpha1.Bucket) { b.Spec.ServerRef.Name = "another-store" })
	a2 := h.access("wrong-store", other.Name, nil)
	if err := h.reconcile(ar, a2); !errors.Is(err, ErrBucketNotUsable) || !strings.Contains(err.Error(), "another-store") {
		t.Errorf("bucket on another store: %v", err)
	}

	// A Bucket that has no bucket yet is not usable.
	fresh := h.bucket("fresh", nil)
	a3 := h.access("too-early", fresh.Name, nil)
	if err := h.reconcile(ar, a3); !errors.Is(err, ErrBucketNotUsable) || !strings.Contains(err.Error(), "no bucket yet") {
		t.Errorf("bucket without a name: %v", err)
	}
	h.reconcileOK(br, fresh)
	h.reconcileOK(ar, a3)
	h.fake.want(t, "ApplyAccess")

	// Deleting an access does not need the Bucket (it may be gone first).
	h.must(env.Client.Delete(context.Background(), a))
	h.reconcileOK(ar, a)
	if !h.gone(a) {
		t.Error("the claim is still there")
	}

	// A failing read of the Bucket is an error.
	boom := errors.New("api server down")
	wc, err := client.NewWithWatch(env.Config, client.Options{Scheme: env.Scheme})
	h.must(err)
	cfg := h.cfg
	cfg.Client = interceptor.NewClient(wc, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*s3v1alpha1.Bucket); ok {
				return boom
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	if err := h.reconcile(&AccessReconciler{cfg}, a2); !errors.Is(err, boom) {
		t.Errorf("bucket read: %v", err)
	}
}

// A claim that does not exist is no error, and an unreachable store is a
// condition.
func TestAccessReconcileOfMissingClaimAndDownStore(t *testing.T) {
	h := newHarness(t, common.DeletionRetain, nil)
	ar := &AccessReconciler{h.cfg}
	h.reconcileOK(ar, &s3v1alpha1.BucketAccess{ObjectMeta: metav1.ObjectMeta{Name: "never", Namespace: h.ns}})

	h.reconcileOK(&BucketReconciler{h.cfg}, h.bucket("uploads", nil))
	a := h.access("down", "uploads", nil)
	h.fake.fail["User"] = errStore
	if err := h.reconcile(ar, a); !errors.Is(err, errStore) {
		t.Errorf("user lookup: %v", err)
	}
}

// A prefix with a wildcard or a policy variable would widen the policy. The
// CRD rule already keeps such a prefix out of the API server, so this checks
// the controller's own guard (defence in depth) directly.
func TestAccessRefusesAWideningPrefix(t *testing.T) {
	e := accessExt(t, newMemStore())
	for _, prefix := range []string{"*", "a?b", "${aws:username}/"} {
		spec := s3v1alpha1.BucketAccessSpec{Prefix: prefix}
		if _, _, err := e.policy(spec); !errors.Is(err, ErrInvalidPrefix) {
			t.Errorf("policy %q: %v", prefix, err)
		}
		if _, err := e.params(spec); !errors.Is(err, ErrInvalidPrefix) {
			t.Errorf("params %q: %v", prefix, err)
		}
	}
	if _, _, err := e.policy(s3v1alpha1.BucketAccessSpec{Prefix: "team/a/"}); err != nil {
		t.Errorf("a plain prefix: %v", err)
	}
}

func accessExt(t *testing.T, store *memStore) *accessExternal {
	t.Helper()
	be := &backend{store: store}
	claim := &s3v1alpha1.BucketAccess{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "tenant", UID: "uid-1"}}
	e := newAccessExternal(be, claim)
	e.bucket = "bkt"
	e.persist = func(context.Context) error { return nil }
	return e
}

// Every call to the store can fail, and the failure reaches the framework
// unchanged.
func TestAccessStoreFailures(t *testing.T) {
	ctx := context.Background()
	spec := s3v1alpha1.BucketAccessSpec{}
	for _, method := range []string{"ApplyAccess", "IssueKey"} {
		store := newMemStore()
		store.fail[method] = errStore
		e := accessExt(t, store)
		if _, err := e.Create(ctx, spec); !errors.Is(err, errStore) {
			t.Errorf("create, %s: %v", method, err)
		}
		store.fail = map[string]error{}
		cur, err := accessState(ctx, e, spec)
		if err != nil {
			t.Fatal(err)
		}
		store.fail[method] = errStore
		if _, err := e.Update(ctx, spec, cur); !errors.Is(err, errStore) {
			t.Errorf("update, %s: %v", method, err)
		}
	}

	store := newMemStore()
	e := accessExt(t, store)
	store.fail["User"] = errStore
	if _, err := e.Observe(ctx, spec); !errors.Is(err, errStore) {
		t.Errorf("observe, User: %v", err)
	}
	store.fail = map[string]error{}
	if _, err := e.Create(ctx, spec); err != nil {
		t.Fatal(err)
	}
	store.fail["KeyExists"] = errStore
	if _, err := e.Observe(ctx, spec); !errors.Is(err, errStore) {
		t.Errorf("observe, KeyExists: %v", err)
	}
	store.fail = map[string]error{}

	// Persisting the intent fails: nothing is created.
	store2 := newMemStore()
	e2 := accessExt(t, store2)
	e2.persist = func(context.Context) error { return errStore }
	if _, err := e2.Create(ctx, spec); !errors.Is(err, errStore) {
		t.Errorf("persist: %v", err)
	}
	store2.mu.Lock()
	if len(store2.calls) != 0 {
		t.Errorf("calls = %v", store2.calls)
	}
	store2.mu.Unlock()

	// An invalid prefix is refused by Create, Update and the rotation.
	bad := s3v1alpha1.BucketAccessSpec{Prefix: "a?"}
	if _, err := e.Create(ctx, bad); !errors.Is(err, ErrInvalidPrefix) {
		t.Errorf("create: %v", err)
	}
	if _, err := e.Update(ctx, bad, AccessState{}); !errors.Is(err, ErrInvalidPrefix) {
		t.Errorf("update: %v", err)
	}

	// Observe of an invalid prefix is not an error: the access is simply never
	// up to date, so deleting it works.
	obs, err := e.Observe(ctx, bad)
	if err != nil || !obs.Exists || obs.UpToDate {
		t.Errorf("observe with a bad prefix: %+v, %v", obs, err)
	}
	if err := e.Delete(ctx, obs.Current); err != nil {
		t.Error(err)
	}
	store.fail["DeleteAccess"] = errStore
	if err := e.Delete(ctx, AccessState{}); !errors.Is(err, errStore) {
		t.Errorf("delete: %v", err)
	}
}

func accessState(ctx context.Context, e *accessExternal, spec s3v1alpha1.BucketAccessSpec) (AccessState, error) {
	if _, err := e.Create(ctx, spec); err != nil {
		return AccessState{}, err
	}
	obs, err := e.Observe(ctx, spec)
	return obs.Current, err
}

func TestAccessRotationSteps(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	e := accessExt(t, store)
	spec := s3v1alpha1.BucketAccessSpec{}

	data, gen, err := e.RotateBegin(ctx, spec, AccessState{}, lifecycle.RotationState{Sequence: 2})
	next := s3client.AccessKey("uid-1", 3)
	if err != nil || string(gen) != next || string(data["accessKeyId"]) != next || e.claim.GetExternalID() != next {
		t.Errorf("begin: %v, %v, %v", gen, data, err)
	}
	// The same call issues the same key again.
	if _, again, _ := e.RotateBegin(ctx, spec, AccessState{}, lifecycle.RotationState{Sequence: 2}); again != gen {
		t.Errorf("begin again = %v, want %v", again, gen)
	}
	store.fail["IssueKey"] = errStore
	if _, _, err := e.RotateBegin(ctx, spec, AccessState{}, lifecycle.RotationState{Sequence: 2}); !errors.Is(err, errStore) {
		t.Errorf("begin, IssueKey: %v", err)
	}

	// Finish retires the generation before the active one; the first rotation
	// has sequence 1 and retires the key Create issued.
	store.fail = map[string]error{}
	store.keys[s3client.AccessKey("uid-1", 2)] = "old"
	if err := e.RotateFinish(ctx, spec, AccessState{}, lifecycle.RotationState{Sequence: 3}); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.keys[s3client.AccessKey("uid-1", 2)]; ok {
		t.Error("the previous key is still there")
	}
	store.reset()
	if err := e.RotateFinish(ctx, spec, AccessState{}, lifecycle.RotationState{}); err != nil {
		t.Fatal(err)
	}
	store.wantNot(t, "DeleteKey")
	store.fail["DeleteKey"] = errStore
	if err := e.RotateFinish(ctx, spec, AccessState{}, lifecycle.RotationState{Sequence: 1}); !errors.Is(err, errStore) {
		t.Errorf("finish: %v", err)
	}
}

func TestAccessBindingData(t *testing.T) {
	e := accessExt(t, newMemStore())
	e.be.conn.Endpoint = "https://rustfs.example.test:9000"
	e.be.conn.CABundle = []byte("PEM")
	d := e.data("KEY", "secret")
	if string(d["pathStyle"]) != "true" || string(d["sslrootcert"]) != "PEM" || string(d["region"]) != "us-east-1" ||
		string(d["bucket"]) != "bkt" || string(d["accessKeyId"]) != "KEY" || string(d["secretAccessKey"]) != "secret" {
		t.Errorf("data = %v", d)
	}
	yes, no := true, false
	e.be.spec.PathStyle = &yes
	if string(e.data("K", "s")["pathStyle"]) != "true" {
		t.Error("pathStyle true")
	}
	e.be.spec.PathStyle = &no
	if string(e.data("K", "s")["pathStyle"]) != "false" {
		t.Error("pathStyle false")
	}
}

func TestNewAccessExternalKeepsTheRecordedName(t *testing.T) {
	claim := &s3v1alpha1.BucketAccess{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "tenant"}}
	e := newAccessExternal(&backend{}, claim)
	if e.user != s3client.UserName("tenant", "app") || claim.GetExternalName() != e.user {
		t.Errorf("user = %q", e.user)
	}
	claim.SetExternalName("kept")
	if e = newAccessExternal(&backend{}, claim); e.user != "kept" {
		t.Errorf("user = %q", e.user)
	}
}
