package controller

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
	"github.com/craftainer-labs/lib-operator-go/pkg/testenv"

	s3v1alpha1 "github.com/craftainer-labs/operator-s3/api/v1alpha1"
	"github.com/craftainer-labs/operator-s3/internal/egress"
	"github.com/craftainer-labs/operator-s3/internal/s3client"
)

// The reconcile flow runs against envtest's kube-apiserver with an in-memory
// store behind the Store interface, so it needs no RustFS. The real RustFS
// integration lane is test/integration. The suite needs the binaries
// scripts/go-coverage.sh and `make test` fetch with setup-envtest.

var env *testenv.Env

func TestMain(m *testing.M) {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(s3v1alpha1.AddToScheme(scheme))
	var err error
	env, err = testenv.Start(context.Background(), testenv.Options{
		Scheme: scheme, CRDPaths: []string{filepath.Join("..", "..", "config", "crd", "bases")},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot start envtest:", err)
		os.Exit(1)
	}
	code := m.Run()
	if err := env.Stop(); err != nil {
		fmt.Fprintln(os.Stderr, "cannot stop envtest:", err)
		code = 1
	}
	os.Exit(code)
}

var errStore = errors.New("store failed")

// memUser is an IAM user of the in-memory store.
type memUser struct {
	enabled  bool
	policies []string
}

// memStore is a RustFS in memory behind Store. It records every call and
// fails the methods named in fail with the error given.
type memStore struct {
	mu      sync.Mutex
	calls   []string
	buckets map[string]s3client.BucketState
	users   map[string]*memUser
	keys    map[string]string
	docs    map[string]string
	fail    map[string]error
	targets []s3client.Target
}

func newMemStore() *memStore {
	return &memStore{
		buckets: map[string]s3client.BucketState{}, users: map[string]*memUser{}, keys: map[string]string{},
		docs: map[string]string{}, fail: map[string]error{},
	}
}

// enter records a call and returns the error injected for it.
func (m *memStore) enter(call string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, call)
	name, _, _ := strings.Cut(call, " ")
	return m.fail[name]
}

func (m *memStore) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = nil
}

func (m *memStore) joined() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return strings.Join(m.calls, "\n")
}

func (m *memStore) want(t *testing.T, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if !strings.Contains(m.joined(), s) {
			t.Errorf("no call contains %q; calls:\n%s", s, m.joined())
		}
	}
}

func (m *memStore) wantNot(t *testing.T, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if strings.Contains(m.joined(), s) {
			t.Errorf("a call contains %q; calls:\n%s", s, m.joined())
		}
	}
}

func (m *memStore) Bucket(_ context.Context, name string) (s3client.BucketState, error) {
	if err := m.enter("Bucket " + name); err != nil {
		return s3client.BucketState{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.buckets[name], nil
}

func (m *memStore) CreateBucket(_ context.Context, p s3client.BucketParams) error {
	if err := m.enter(fmt.Sprintf("CreateBucket %s lock=%t versioning=%t quota=%d rules=%d", p.Name, p.ObjectLock, p.Versioning, p.Quota, len(p.Rules))); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.buckets[p.Name] = s3client.BucketState{
		Exists: true, Tags: map[string]string{s3client.MarkerTag: p.Marker}, Versioning: p.Versioning, Quota: p.Quota, Rules: p.Rules,
	}
	return nil
}

func (m *memStore) UpdateBucket(_ context.Context, p s3client.BucketParams, _ s3client.BucketState) error {
	if err := m.enter(fmt.Sprintf("UpdateBucket %s versioning=%t quota=%d rules=%d", p.Name, p.Versioning, p.Quota, len(p.Rules))); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.buckets[p.Name] = s3client.BucketState{
		Exists: true, Tags: map[string]string{s3client.MarkerTag: p.Marker}, Versioning: p.Versioning, Quota: p.Quota, Rules: p.Rules,
	}
	return nil
}

func (m *memStore) DeleteBucket(_ context.Context, name string) error {
	if err := m.enter("DeleteBucket " + name); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.buckets, name)
	return nil
}

func (m *memStore) User(_ context.Context, name string) (s3client.UserState, error) {
	if err := m.enter("User " + name); err != nil {
		return s3client.UserState{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	u := m.users[name]
	if u == nil {
		return s3client.UserState{}, nil
	}
	return s3client.UserState{Exists: true, Enabled: u.enabled, Policies: slices.Clone(u.policies)}, nil
}

func (m *memStore) ApplyAccess(_ context.Context, p s3client.AccessParams, _ s3client.UserState) error {
	if err := m.enter("ApplyAccess " + p.User + " " + p.Policy); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.users[p.User] = &memUser{enabled: true, policies: []string{p.Policy}}
	m.docs[p.Policy] = string(p.PolicyDoc)
	return nil
}

func (m *memStore) KeyExists(_ context.Context, key string) (bool, error) {
	if err := m.enter("KeyExists " + key); err != nil {
		return false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.keys[key]
	return ok, nil
}

func (m *memStore) IssueKey(_ context.Context, user, key, secret string) error {
	if err := m.enter("IssueKey " + user + " " + key); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.keys[key] = secret
	return nil
}

func (m *memStore) DeleteKey(_ context.Context, key string) error {
	if err := m.enter("DeleteKey " + key); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.keys, key)
	return nil
}

func (m *memStore) OwnedPolicies(_ context.Context, user string) ([]string, error) {
	if err := m.enter("OwnedPolicies " + user); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var owned []string
	for name := range m.docs {
		if s3client.OwnsPolicy(user, name) {
			owned = append(owned, name)
		}
	}
	slices.Sort(owned)
	return owned, nil
}

func (m *memStore) DeleteAccess(_ context.Context, user string, keys []string) error {
	if err := m.enter("DeleteAccess " + user + " " + strings.Join(keys, ",")); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.users, user)
	for _, k := range keys {
		delete(m.keys, k)
	}
	for name := range m.docs {
		if s3client.OwnsPolicy(user, name) {
			delete(m.docs, name)
		}
	}
	return nil
}

// harness is one test's namespaces, store, recorder and in-memory RustFS.
type harness struct {
	t      *testing.T
	ns     string // the tenant namespace
	ops    string // the operator namespace, holding the admin Secret
	server *s3v1alpha1.ClusterObjectStore
	fake   *memStore
	rec    *testenv.Recorder
	cfg    Config
	now    time.Time
}

func newHarness(t *testing.T, policy common.DeletionPolicy, mutate func(*s3v1alpha1.ClusterObjectStore)) *harness {
	t.Helper()
	h := &harness{t: t, ns: env.Namespace(t, nil), ops: env.Namespace(t, nil), fake: newMemStore(), rec: &testenv.Recorder{}, now: time.Now()}
	name := "rustfs-" + strings.ToLower(rand.Text())[:10]
	admin := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-admin", Namespace: h.ops},
		Data:       map[string][]byte{"accessKey": []byte("admin-ak"), "secretKey": []byte("admin-sk")},
	}
	srv := &s3v1alpha1.ClusterObjectStore{ObjectMeta: metav1.ObjectMeta{Name: name}}
	srv.Spec.Endpoint = "https://rustfs.example.test:9000"
	srv.Spec.AdminCredentialsSecretRef = common.AdminSecretRef{Name: admin.Name, Namespace: h.ops}
	srv.Spec.AllowedNamespaces = common.AllowedNamespaces{From: common.NamespacesAll}
	if mutate != nil {
		mutate(srv)
	}
	h.must(env.Client.Create(context.Background(), admin))
	h.must(env.Client.Create(context.Background(), srv))
	t.Cleanup(func() { _ = env.Client.Delete(context.Background(), srv) })
	h.server = srv
	h.cfg = Config{
		Client: env.Client, APIReader: env.Client, Recorder: h.rec, ManagedBy: "operator-s3-test",
		OperatorNamespace: h.ops, DefaultDeletionPolicy: policy, Egress: egress.NewPolicy(egress.ModeStrict),
		NewStore: func(tg s3client.Target) (Store, error) {
			h.fake.mu.Lock()
			defer h.fake.mu.Unlock()
			h.fake.targets = append(h.fake.targets, tg)
			return h.fake, nil
		},
		Now: func() time.Time { return h.now },
	}
	return h
}

func (h *harness) must(err error) {
	h.t.Helper()
	if err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) ref() common.ServerRef {
	return common.ServerRef{Kind: s3v1alpha1.KindClusterObjectStore, Name: h.server.Name}
}

func (h *harness) bucket(name string, mutate func(*s3v1alpha1.Bucket)) *s3v1alpha1.Bucket {
	h.t.Helper()
	b := &s3v1alpha1.Bucket{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: h.ns}}
	b.Spec.ServerRef = h.ref()
	if mutate != nil {
		mutate(b)
	}
	h.must(env.Client.Create(context.Background(), b))
	return b
}

func (h *harness) access(name, bucket string, mutate func(*s3v1alpha1.BucketAccess)) *s3v1alpha1.BucketAccess {
	h.t.Helper()
	a := &s3v1alpha1.BucketAccess{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: h.ns}}
	a.Spec.ServerRef = h.ref()
	a.Spec.BucketRef = s3v1alpha1.BucketRef{Name: bucket}
	if mutate != nil {
		mutate(a)
	}
	h.must(env.Client.Create(context.Background(), a))
	return a
}

type reconciler interface {
	Reconcile(context.Context, ctrl.Request) (ctrl.Result, error)
}

func (h *harness) reconcile(r reconciler, o client.Object) error {
	h.t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(o)})
	return err
}

func (h *harness) reconcileOK(r reconciler, o client.Object) {
	h.t.Helper()
	h.must(h.reconcile(r, o))
}

func (h *harness) get(o client.Object) {
	h.t.Helper()
	h.must(env.Client.Get(context.Background(), client.ObjectKeyFromObject(o), o))
}

func (h *harness) gone(o client.Object) bool {
	h.t.Helper()
	err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(o), o)
	return apierrors.IsNotFound(err)
}

// patch applies mutate to a fresh copy of o and sends it as a merge patch.
func (h *harness) patch(o client.Object, mutate func()) {
	h.t.Helper()
	h.get(o)
	p := client.MergeFrom(o.DeepCopyObject().(client.Object))
	mutate()
	h.must(env.Client.Patch(context.Background(), o, p))
}

// condition returns the status, reason and message of a condition of a claim.
func condition(conds []metav1.Condition, typ string) (metav1.ConditionStatus, string, string) {
	c := meta.FindStatusCondition(conds, typ)
	if c == nil {
		return "", "", ""
	}
	return c.Status, c.Reason, c.Message
}

func (h *harness) wantCondition(conds []metav1.Condition, typ string, status metav1.ConditionStatus, reason string) string {
	h.t.Helper()
	s, r, msg := condition(conds, typ)
	if s != status || r != reason {
		h.t.Errorf("condition %s = %s/%s (%s), want %s/%s", typ, s, r, msg, status, reason)
	}
	return msg
}
