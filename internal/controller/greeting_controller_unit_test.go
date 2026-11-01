package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	samplev1alpha1 "github.com/craftainer/template-k8s-operator/api/v1alpha1"
)

// Unit tests: the reconciler against a fake client, with faults injected into
// single calls so every error branch runs. The envtest suite
// (suite_test.go) covers the same code against a real API server.

var errBoom = errors.New("boom")

// faults lists the calls that fail. A nil field leaves the call alone.
type faults struct {
	getGreeting, getConfigMap        error
	createConfigMap, updateConfigMap error
	deleteConfigMap                  error
	updateGreeting, updateStatus     error
}

func (f faults) funcs() interceptor.Funcs {
	return interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			switch obj.(type) {
			case *samplev1alpha1.Greeting:
				if f.getGreeting != nil {
					return f.getGreeting
				}
			case *corev1.ConfigMap:
				if f.getConfigMap != nil {
					return f.getConfigMap
				}
			}
			return c.Get(ctx, key, obj, opts...)
		},
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if f.createConfigMap != nil {
				return f.createConfigMap
			}
			return c.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			switch obj.(type) {
			case *samplev1alpha1.Greeting:
				if f.updateGreeting != nil {
					return f.updateGreeting
				}
			case *corev1.ConfigMap:
				if f.updateConfigMap != nil {
					return f.updateConfigMap
				}
			}
			return c.Update(ctx, obj, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if f.deleteConfigMap != nil {
				return f.deleteConfigMap
			}
			return c.Delete(ctx, obj, opts...)
		},
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if f.updateStatus != nil {
				return f.updateStatus
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	}
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := samplev1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func newGreeting() *samplev1alpha1.Greeting {
	return &samplev1alpha1.Greeting{
		ObjectMeta: metav1.ObjectMeta{Name: "hello", Namespace: "default", UID: "uid-1", Generation: 3},
		Spec:       samplev1alpha1.GreetingSpec{Message: "Hello", DeletionPolicy: samplev1alpha1.DeletionPolicyDelete},
	}
}

type fixture struct {
	r        *GreetingReconciler
	c        client.Client
	recorder *events.FakeRecorder
}

func newFixture(t *testing.T, f faults, objs ...client.Object) *fixture {
	t.Helper()
	s := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
		WithStatusSubresource(&samplev1alpha1.Greeting{}).WithInterceptorFuncs(f.funcs()).Build()
	rec := events.NewFakeRecorder(32)
	return &fixture{r: &GreetingReconciler{Client: c, Scheme: s, Recorder: rec}, c: c, recorder: rec}
}

func (fx *fixture) reconcile(g *samplev1alpha1.Greeting) (ctrl.Result, error) {
	return fx.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(g)})
}

func (fx *fixture) greeting(t *testing.T) *samplev1alpha1.Greeting {
	t.Helper()
	g := &samplev1alpha1.Greeting{}
	if err := fx.c.Get(context.Background(), client.ObjectKeyFromObject(newGreeting()), g); err != nil {
		t.Fatal(err)
	}
	return g
}

func (fx *fixture) drainEvents() string {
	var out []string
	for {
		select {
		case e := <-fx.recorder.Events:
			out = append(out, e)
		default:
			return strings.Join(out, "\n")
		}
	}
}

func TestReconcileProvisionsAndReportsReady(t *testing.T) {
	g := NewWithT(t)
	fx := newFixture(t, faults{}, newGreeting())

	res, err := fx.reconcile(newGreeting())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(DefaultPollInterval))

	cm := &corev1.ConfigMap{}
	g.Expect(fx.c.Get(context.Background(), client.ObjectKeyFromObject(newGreeting()), cm)).To(Succeed())
	g.Expect(cm.Data).To(HaveKeyWithValue(MessageKey, "Hello"))
	g.Expect(metav1.IsControlledBy(cm, fx.greeting(t))).To(BeTrue())

	got := fx.greeting(t)
	g.Expect(controllerutil.ContainsFinalizer(got, Finalizer)).To(BeTrue())
	g.Expect(got.Status.ConfigMapName).To(Equal("hello"))
	for _, ct := range []string{ConditionReady, ConditionSynced} {
		c := meta.FindStatusCondition(got.Status.Conditions, ct)
		g.Expect(c).NotTo(BeNil(), ct)
		g.Expect(c.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(c.ObservedGeneration).To(Equal(int64(3)))
	}
	g.Expect(fx.drainEvents()).To(ContainSubstring("Normal Synced"))

	// A second pass changes nothing and records no Event.
	_, err = fx.reconcile(newGreeting())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(fx.drainEvents()).To(BeEmpty())
}

func TestReconcileUpdatesTheConfigMapWhenTheSpecChanges(t *testing.T) {
	g := NewWithT(t)
	fx := newFixture(t, faults{}, newGreeting())
	_, err := fx.reconcile(newGreeting())
	g.Expect(err).NotTo(HaveOccurred())

	got := fx.greeting(t)
	got.Spec.Message = "Hi"
	g.Expect(fx.c.Update(context.Background(), got)).To(Succeed())
	_, err = fx.reconcile(newGreeting())
	g.Expect(err).NotTo(HaveOccurred())

	cm := &corev1.ConfigMap{}
	g.Expect(fx.c.Get(context.Background(), client.ObjectKeyFromObject(newGreeting()), cm)).To(Succeed())
	g.Expect(cm.Data[MessageKey]).To(Equal("Hi"))
}

func TestReconcileIgnoresAMissingGreeting(t *testing.T) {
	g := NewWithT(t)
	fx := newFixture(t, faults{})
	res, err := fx.reconcile(newGreeting())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))
}

func TestReconcileReturnsGetErrors(t *testing.T) {
	g := NewWithT(t)
	fx := newFixture(t, faults{getGreeting: errBoom}, newGreeting())
	_, err := fx.reconcile(newGreeting())
	g.Expect(err).To(MatchError(errBoom))
}

func TestReconcileReturnsFinalizerErrors(t *testing.T) {
	g := NewWithT(t)
	fx := newFixture(t, faults{updateGreeting: errBoom}, newGreeting())
	_, err := fx.reconcile(newGreeting())
	g.Expect(err).To(MatchError(errBoom))
}

func TestReconcileReportsASyncFailureInStatus(t *testing.T) {
	g := NewWithT(t)
	fx := newFixture(t, faults{createConfigMap: errBoom}, newGreeting())
	_, err := fx.reconcile(newGreeting())
	g.Expect(err).To(MatchError(errBoom))

	c := meta.FindStatusCondition(fx.greeting(t).Status.Conditions, ConditionReady)
	g.Expect(c).NotTo(BeNil())
	g.Expect(c.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(c.Reason).To(Equal("ReconcileError"))
	g.Expect(fx.drainEvents()).To(ContainSubstring("Warning ReconcileError"))
}

func TestReconcileStillReturnsTheSyncErrorWhenStatusCannotBeWritten(t *testing.T) {
	g := NewWithT(t)
	fx := newFixture(t, faults{createConfigMap: errBoom, updateStatus: errors.New("status down")}, newGreeting())
	_, err := fx.reconcile(newGreeting())
	g.Expect(err).To(MatchError(errBoom))
}

func TestReconcileRefusesAConfigMapItDoesNotOwn(t *testing.T) {
	g := NewWithT(t)
	foreign := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "hello", Namespace: "default"},
		Data:       map[string]string{MessageKey: "mine"},
	}
	fx := newFixture(t, faults{}, newGreeting(), foreign)
	_, err := fx.reconcile(newGreeting())
	g.Expect(err).To(MatchError(ErrNameCollision))

	c := meta.FindStatusCondition(fx.greeting(t).Status.Conditions, ConditionSynced)
	g.Expect(c).NotTo(BeNil())
	g.Expect(c.Reason).To(Equal("NameCollision"))

	cm := &corev1.ConfigMap{}
	g.Expect(fx.c.Get(context.Background(), client.ObjectKeyFromObject(foreign), cm)).To(Succeed())
	g.Expect(cm.Data[MessageKey]).To(Equal("mine"))
}

func TestReconcileReturnsStatusErrors(t *testing.T) {
	g := NewWithT(t)
	fx := newFixture(t, faults{updateStatus: errBoom}, newGreeting())
	_, err := fx.reconcile(newGreeting())
	g.Expect(err).To(MatchError(errBoom))
}

func TestPollInterval(t *testing.T) {
	cases := map[string]struct {
		annotations map[string]string
		want        time.Duration
		warning     string
	}{
		"absent":    {nil, DefaultPollInterval, ""},
		"valid":     {map[string]string{PollIntervalAnnotation: "30s"}, 30 * time.Second, ""},
		"minimum":   {map[string]string{PollIntervalAnnotation: "10s"}, MinPollInterval, ""},
		"too short": {map[string]string{PollIntervalAnnotation: "1ms"}, MinPollInterval, "raising"},
		"garbage":   {map[string]string{PollIntervalAnnotation: "soon"}, DefaultPollInterval, "ignoring"},
		"negative":  {map[string]string{PollIntervalAnnotation: "-1m"}, DefaultPollInterval, "ignoring"},
		"zero":      {map[string]string{PollIntervalAnnotation: "0s"}, DefaultPollInterval, "ignoring"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			g := NewWithT(t)
			greeting := newGreeting()
			greeting.Annotations = tc.annotations
			got, warning := pollInterval(greeting)
			g.Expect(got).To(Equal(tc.want))
			if tc.warning == "" {
				g.Expect(warning).To(BeEmpty())
			} else {
				g.Expect(warning).To(ContainSubstring(tc.warning))
			}
		})
	}
}

func TestReconcileWarnsAboutABadPollIntervalOncePerGeneration(t *testing.T) {
	g := NewWithT(t)
	greeting := newGreeting()
	greeting.Annotations = map[string]string{PollIntervalAnnotation: "1ms"}
	fx := newFixture(t, faults{}, greeting)

	res, err := fx.reconcile(greeting)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(MinPollInterval))
	g.Expect(fx.drainEvents()).To(ContainSubstring("Warning InvalidPollInterval"))

	// Same generation again: still clamped, no second Event.
	res, err = fx.reconcile(greeting)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(MinPollInterval))
	g.Expect(fx.drainEvents()).NotTo(ContainSubstring("InvalidPollInterval"))

	// A new generation warns again.
	got := fx.greeting(t)
	got.Generation++
	g.Expect(fx.c.Update(context.Background(), got)).To(Succeed())
	_, err = fx.reconcile(greeting)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(fx.drainEvents()).To(ContainSubstring("Warning InvalidPollInterval"))
}

func TestReconcileRequeuesQuietlyOnConflicts(t *testing.T) {
	conflict := apierrors.NewConflict(schema.GroupResource{Resource: "greetings"}, "hello", errBoom)
	cases := map[string]faults{
		"finalizer": {updateGreeting: conflict},
		"configmap": {createConfigMap: conflict},
		"status":    {updateStatus: conflict},
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			g := NewWithT(t)
			fx := newFixture(t, f, newGreeting())
			before := testutil.ToFloat64(reconcileTotal.WithLabelValues("error"))
			res, err := fx.reconcile(newGreeting())
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(res.RequeueAfter).To(Equal(conflictRequeue))
			g.Expect(fx.drainEvents()).NotTo(ContainSubstring("Warning"))
			g.Expect(testutil.ToFloat64(reconcileTotal.WithLabelValues("error"))).To(Equal(before))
		})
	}
}

func TestReconcileReAdoptsAReleasedConfigMap(t *testing.T) {
	g := NewWithT(t)
	retained := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "hello", Namespace: "default", Labels: map[string]string{ManagedByLabel: ManagedByValue}},
		Data:       map[string]string{MessageKey: "old"},
	}
	fx := newFixture(t, faults{}, newGreeting(), retained)
	_, err := fx.reconcile(newGreeting())
	g.Expect(err).NotTo(HaveOccurred())

	cm := &corev1.ConfigMap{}
	g.Expect(fx.c.Get(context.Background(), client.ObjectKeyFromObject(retained), cm)).To(Succeed())
	g.Expect(cm.Data[MessageKey]).To(Equal("Hello"))
	g.Expect(metav1.IsControlledBy(cm, fx.greeting(t))).To(BeTrue())
}

func TestReconcileDoesNotStealAControlledLabelledConfigMap(t *testing.T) {
	g := NewWithT(t)
	other := newGreeting()
	other.UID = "uid-other"
	s := testScheme(t)
	cm := ownedConfigMap(other, s)
	cm.Labels = map[string]string{ManagedByLabel: ManagedByValue}
	fx := newFixture(t, faults{}, newGreeting(), cm)
	_, err := fx.reconcile(newGreeting())
	g.Expect(err).To(MatchError(ErrNameCollision))
}

func TestFinalizeEmitsNoReleaseEventForAnObjectItDidNotRelease(t *testing.T) {
	g := NewWithT(t)
	greeting := deleting(samplev1alpha1.DeletionPolicyDelete)
	foreign := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "hello", Namespace: "default"}}
	fx := newFixture(t, faults{}, greeting.DeepCopy(), foreign)
	greeting = fx.greeting(t)
	g.Expect(fx.r.finalize(context.Background(), greeting)).To(Succeed())
	g.Expect(fx.drainEvents()).To(BeEmpty())
}

// deleting returns a Greeting that is being deleted and still has its finalizer.
func deleting(policy samplev1alpha1.DeletionPolicy) *samplev1alpha1.Greeting {
	g := newGreeting()
	g.Spec.DeletionPolicy = policy
	now := metav1.Now()
	g.DeletionTimestamp = &now
	g.Finalizers = []string{Finalizer}
	return g
}

func ownedConfigMap(owner *samplev1alpha1.Greeting, s *runtime.Scheme) *corev1.ConfigMap {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: owner.Name, Namespace: owner.Namespace}}
	if err := controllerutil.SetControllerReference(owner, cm, s); err != nil {
		panic(err)
	}
	return cm
}

func TestFinalizeDeletesTheConfigMap(t *testing.T) {
	g := NewWithT(t)
	greeting := deleting(samplev1alpha1.DeletionPolicyDelete)
	fx := newFixture(t, faults{}, greeting.DeepCopy())
	g.Expect(fx.c.Create(context.Background(), ownedConfigMap(greeting, fx.r.Scheme))).To(Succeed())
	greeting = fx.greeting(t)

	g.Expect(fx.r.finalize(context.Background(), greeting)).To(Succeed())
	err := fx.c.Get(context.Background(), client.ObjectKeyFromObject(greeting), &corev1.ConfigMap{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
	g.Expect(greeting.Finalizers).To(BeEmpty())
	g.Expect(fx.drainEvents()).To(ContainSubstring("Normal Deleted"))
}

func TestFinalizeRetainsTheConfigMapButReleasesIt(t *testing.T) {
	g := NewWithT(t)
	greeting := deleting(samplev1alpha1.DeletionPolicyRetain)
	fx := newFixture(t, faults{}, greeting.DeepCopy())
	g.Expect(fx.c.Create(context.Background(), ownedConfigMap(greeting, fx.r.Scheme))).To(Succeed())
	greeting = fx.greeting(t)

	g.Expect(fx.r.finalize(context.Background(), greeting)).To(Succeed())
	cm := &corev1.ConfigMap{}
	g.Expect(fx.c.Get(context.Background(), client.ObjectKeyFromObject(greeting), cm)).To(Succeed())
	g.Expect(cm.OwnerReferences).To(BeEmpty())
	g.Expect(greeting.Finalizers).To(BeEmpty())
}

func TestFinalizeLeavesAForeignConfigMapAlone(t *testing.T) {
	g := NewWithT(t)
	greeting := deleting(samplev1alpha1.DeletionPolicyDelete)
	foreign := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "hello", Namespace: "default"}}
	fx := newFixture(t, faults{}, greeting.DeepCopy(), foreign)
	greeting = fx.greeting(t)

	g.Expect(fx.r.finalize(context.Background(), greeting)).To(Succeed())
	g.Expect(fx.c.Get(context.Background(), client.ObjectKeyFromObject(foreign), &corev1.ConfigMap{})).To(Succeed())
	g.Expect(greeting.Finalizers).To(BeEmpty())
}

func TestFinalizeToleratesAMissingConfigMap(t *testing.T) {
	g := NewWithT(t)
	greeting := deleting(samplev1alpha1.DeletionPolicyDelete)
	fx := newFixture(t, faults{}, greeting.DeepCopy())
	greeting = fx.greeting(t)
	g.Expect(fx.r.finalize(context.Background(), greeting)).To(Succeed())
	g.Expect(greeting.Finalizers).To(BeEmpty())
}

func TestFinalizeWithoutOurFinalizerDoesNothing(t *testing.T) {
	g := NewWithT(t)
	greeting := deleting(samplev1alpha1.DeletionPolicyDelete)
	greeting.Finalizers = nil
	fx := newFixture(t, faults{getConfigMap: errBoom})
	g.Expect(fx.r.finalize(context.Background(), greeting)).To(Succeed())
}

func TestFinalizeErrors(t *testing.T) {
	notFound := apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, "hello")
	cases := map[string]struct {
		policy  samplev1alpha1.DeletionPolicy
		f       faults
		scheme  func(*runtime.Scheme) *runtime.Scheme
		wantErr error
	}{
		"get":                 {samplev1alpha1.DeletionPolicyDelete, faults{getConfigMap: errBoom}, nil, errBoom},
		"delete":              {samplev1alpha1.DeletionPolicyDelete, faults{deleteConfigMap: errBoom}, nil, errBoom},
		"delete already gone": {samplev1alpha1.DeletionPolicyDelete, faults{deleteConfigMap: notFound}, nil, nil},
		"release update":      {samplev1alpha1.DeletionPolicyRetain, faults{updateConfigMap: errBoom}, nil, errBoom},
		"release unknown kind": {samplev1alpha1.DeletionPolicyRetain, faults{},
			func(*runtime.Scheme) *runtime.Scheme { return runtime.NewScheme() }, runtime.NewNotRegisteredErrForKind("", schema.GroupVersionKind{})},
		"finalizer update": {samplev1alpha1.DeletionPolicyDelete, faults{updateGreeting: errBoom}, nil, errBoom},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			g := NewWithT(t)
			greeting := deleting(tc.policy)
			fx := newFixture(t, tc.f, greeting.DeepCopy())
			g.Expect(fx.c.Create(context.Background(), ownedConfigMap(greeting, fx.r.Scheme))).To(Succeed())
			greeting = fx.greeting(t)
			if tc.scheme != nil {
				fx.r.Scheme = tc.scheme(fx.r.Scheme)
			}
			err := fx.r.finalize(context.Background(), greeting)
			switch {
			case tc.wantErr == nil:
				g.Expect(err).NotTo(HaveOccurred())
			case name == "release unknown kind":
				g.Expect(err).To(HaveOccurred())
				g.Expect(runtime.IsNotRegisteredError(err)).To(BeTrue())
			default:
				g.Expect(err).To(MatchError(tc.wantErr))
			}
		})
	}
}

func TestCountReconcile(t *testing.T) {
	g := NewWithT(t)
	ok, bad := testutil.ToFloat64(reconcileTotal.WithLabelValues("success")), testutil.ToFloat64(reconcileTotal.WithLabelValues("error"))
	countReconcile(nil)
	countReconcile(errBoom)
	g.Expect(testutil.ToFloat64(reconcileTotal.WithLabelValues("success"))).To(Equal(ok + 1))
	g.Expect(testutil.ToFloat64(reconcileTotal.WithLabelValues("error"))).To(Equal(bad + 1))
}
