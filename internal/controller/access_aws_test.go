package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
	"github.com/craftainer-labs/lib-operator-go/pkg/conditions"
	"github.com/craftainer-labs/lib-operator-go/pkg/lifecycle"

	s3v1alpha1 "github.com/craftainer-labs/operator-s3/api/v1alpha1"
	"github.com/craftainer-labs/operator-s3/internal/s3client"
)

// A BucketAccess on an AWS store: an IAM user tagged with the claim, one access
// key that IAM generates, a rotation that uses the user's second key, and a
// delete that removes the keys with the user.
func TestAccessOnAWS(t *testing.T) {
	h := newAWSHarness(t, nil)
	br, ar := &BucketReconciler{h.cfg}, &AccessReconciler{h.cfg}
	h.reconcileOK(br, h.bucket("uploads", nil))
	bucket := s3client.BucketName(h.ns, "uploads")

	grace := metav1.Duration{Duration: time.Hour}
	interval := metav1.Duration{Duration: 24 * time.Hour}
	a := h.access("uploads-app", "uploads", func(a *s3v1alpha1.BucketAccess) {
		a.Spec.Access = s3v1alpha1.AccessReadOnly
		a.Spec.Prefix = "app/"
		a.Spec.RotationPolicy = &common.RotationPolicy{Interval: &interval, GracePeriod: &grace}
	})
	user := s3client.IAMUserName(h.ns, "uploads-app")

	// Create: the user, tagged with the claim, its policy and the first key.
	h.fake.reset()
	h.reconcileOK(ar, a)
	h.get(a)
	h.fake.want(t, "ApplyAccess "+user, "NewKey "+user)
	if a.Status.ExternalName != user || a.Status.Rotation == nil || a.Status.Rotation.Active == "" {
		t.Fatalf("status = %+v", a.Status)
	}
	key0 := a.Status.Rotation.Active
	if got := h.fake.users[user]; got == nil || got.marker != claimID(a) {
		t.Errorf("user = %+v", got)
	}
	data := bindingSecret(h, a)
	if data["accessKeyId"] != key0 || data["secretAccessKey"] != h.fake.generated[0].secret || data["cloud"] != "aws" ||
		data["bucket"] != bucket || data["prefix"] != "app/" || data["region"] != "eu-west-1" || data["pathStyle"] != "false" ||
		data["endpoint"] != "https://s3.eu-west-1.amazonaws.com" || data["type"] != "s3" {
		t.Errorf("binding = %v", data)
	}
	h.rec.AssertNoSecret(t, data["secretAccessKey"])

	// The next reconcile records the user's ARN, and then nothing is written.
	h.reconcileOK(ar, a)
	h.get(a)
	if a.Status.ExternalID != "arn:aws:iam::1:user/"+user {
		t.Errorf("externalId = %q", a.Status.ExternalID)
	}
	h.fake.reset()
	h.reconcileOK(ar, a)
	h.fake.wantNot(t, "ApplyAccess", "NewKey", "RemoveKey", "DeleteAccess")

	// Rotation: the annotation issues the user's second key; the first stays
	// valid during the grace period.
	h.patch(a, func() { a.Annotations = map[string]string{lifecycle.AnnotationRotate: "1"} })
	h.fake.reset()
	h.reconcileOK(ar, a)
	h.get(a)
	key1 := a.Status.Rotation.Active
	if key1 == "" || key1 == key0 || a.Status.Rotation.Previous != key0 || len(h.fake.generated) != 2 {
		t.Fatalf("rotation = %+v, keys = %v", a.Status.Rotation, h.fake.generated)
	}
	h.fake.wantNot(t, "RemoveKey")
	if data = bindingSecret(h, a); data["accessKeyId"] != key1 || data["secretAccessKey"] != h.fake.generated[1].secret {
		t.Errorf("rotated binding = %v", data)
	}

	// After the grace period the previous key is deleted.
	h.now = h.now.Add(2 * time.Hour)
	h.fake.reset()
	h.reconcileOK(ar, a)
	h.fake.want(t, "RemoveKey "+user+" "+key0)
	if len(h.fake.generated) != 1 || h.fake.generated[0].id != key1 {
		t.Errorf("keys = %v", h.fake.generated)
	}
	h.rec.AssertEvent(t, "Normal", conditions.EventRotationFinished, "")

	// A key a crashed rotation left (its secret is in no Secret) goes before the
	// next one is made, so the user never has more than IAM's two.
	h.fake.generated = append(h.fake.generated, memKey{id: "AKIAORPHAN", secret: "lost"})
	h.patch(a, func() { a.Annotations = map[string]string{lifecycle.AnnotationRotate: "2"} })
	h.fake.reset()
	h.reconcileOK(ar, a)
	h.fake.want(t, "RemoveKey "+user+" AKIAORPHAN", "NewKey "+user)
	if len(h.fake.generated) != 2 {
		t.Errorf("keys = %v", h.fake.generated)
	}

	// A key that was deleted behind our back is issued again.
	h.now = h.now.Add(2 * time.Hour)
	h.reconcileOK(ar, a)
	h.fake.generated = nil
	h.fake.reset()
	h.reconcileOK(ar, a)
	h.fake.want(t, "NewKey "+user)
	h.get(a)
	if len(h.fake.generated) != 1 || a.Status.Rotation.Active != h.fake.generated[0].id {
		t.Errorf("rotation = %+v, keys = %v", a.Status.Rotation, h.fake.generated)
	}

	// Deleting removes the user with its keys.
	h.must(env.Client.Delete(context.Background(), a))
	h.fake.reset()
	h.reconcileOK(ar, a)
	h.fake.want(t, "DeleteAccess "+user)
	if !h.gone(a) || len(h.fake.users) != 0 || len(h.fake.generated) != 0 {
		t.Errorf("gone %v, users %v, keys %v", h.gone(a), h.fake.users, h.fake.generated)
	}
}

// An IAM user is the claim's when it carries the claim as a tag. One that
// carries another claim, or none, is never adopted; one that carries this
// claim is, even when the status lost the record of the create.
func TestAccessOnAWSAdoptsOnlyItsTag(t *testing.T) {
	h := newAWSHarness(t, nil)
	br, ar := &BucketReconciler{h.cfg}, &AccessReconciler{h.cfg}
	h.reconcileOK(br, h.bucket("uploads", nil))
	user := s3client.IAMUserName(h.ns, "app")
	a := h.access("app", "uploads", nil)

	h.fake.users[user] = &memUser{enabled: true, marker: "another/claim/uid"}
	h.reconcileOK(ar, a)
	h.get(a)
	msg := h.wantCondition(a.Status.Conditions, conditions.Synced, metav1.ConditionFalse, conditions.NameCollision)
	if !strings.Contains(msg, ErrExistsUnmanaged.Error()) {
		t.Errorf("message = %q", msg)
	}
	h.fake.wantNot(t, "ApplyAccess", "NewKey")
	if a.Status.ExternalID != "" {
		t.Errorf("a user that is not ours was recorded: %q", a.Status.ExternalID)
	}

	h.fake.users[user].marker = claimID(a)
	h.reconcileOK(ar, a)
	h.get(a)
	if a.Status.ExternalID != "arn:aws:iam::1:user/"+user || a.Status.Rotation == nil || a.Status.Rotation.Active == "" {
		t.Errorf("status = %+v", a.Status)
	}
}

func awsExt(t *testing.T) (*accessExternal, *memStore) {
	t.Helper()
	store := newMemStore()
	return accessExtOn(t, &backend{store: store, gen: store}), store
}

// The name of an AWS user is the long one IAM allows; the key checks need an
// active key.
func TestAWSAccessKeyChecks(t *testing.T) {
	ctx := context.Background()
	e, store := awsExt(t)
	if e.user != s3client.IAMUserName("tenant", "app") || e.activeKey() != "" {
		t.Errorf("user %q, key %q", e.user, e.activeKey())
	}
	if ok, err := e.keyExists(ctx, ""); ok || err != nil {
		t.Errorf("no active key: %v, %v", ok, err)
	}
	if _, err := e.Create(ctx, s3v1alpha1.BucketAccessSpec{}); err != nil {
		t.Fatal(err)
	}
	if ok, err := e.keyExists(ctx, e.activeKey()); !ok || err != nil {
		t.Errorf("active key: %v, %v", ok, err)
	}
	if ok, err := e.keyExists(ctx, "AKIAOTHER"); ok || err != nil {
		t.Errorf("another key: %v, %v", ok, err)
	}
	store.fail["Keys"] = errStore
	if _, err := e.keyExists(ctx, e.activeKey()); !errors.Is(err, errStore) {
		t.Errorf("keys fail: %v", err)
	}
}

// Every call to IAM can fail, and the failure reaches the framework unchanged
// (a lifecycle.PendingError among them). A failed issue records no key.
func TestAWSAccessStoreFailures(t *testing.T) {
	ctx := context.Background()
	spec := s3v1alpha1.BucketAccessSpec{}
	pending := lifecycle.Pending(time.Second, "", "wait")
	for _, tc := range []struct {
		method string
		err    error
	}{{"ApplyAccess", errStore}, {"Keys", errStore}, {"NewKey", pending}, {"RemoveKey", errStore}} {
		e, store := awsExt(t)
		store.generated = []memKey{{id: "AKIAOLD", secret: "s"}}
		store.fail[tc.method] = tc.err
		if _, err := e.Create(ctx, spec); !errors.Is(err, tc.err) {
			t.Errorf("create, %s: %v", tc.method, err)
		}
		if rs := e.claim.GetRotationStatus(); rs.Active != "" {
			t.Errorf("create, %s: recorded key %q", tc.method, rs.Active)
		}
	}

	// A rotation: the leftovers go first, then the new key.
	for _, method := range []string{"Keys", "RemoveKey", "NewKey"} {
		e, store := awsExt(t)
		store.generated = []memKey{{id: "AKIAACTIVE", secret: "s"}, {id: "AKIAORPHAN", secret: "s"}}
		store.fail[method] = errStore
		if _, _, err := e.RotateBegin(ctx, spec, AccessState{}, lifecycle.RotationState{Active: "AKIAACTIVE"}); !errors.Is(err, errStore) {
			t.Errorf("rotate begin, %s: %v", method, err)
		}
	}
	e, store := awsExt(t)
	store.generated = []memKey{{id: "AKIAACTIVE", secret: "s"}, {id: "AKIAORPHAN", secret: "s"}}
	data, gen, err := e.RotateBegin(ctx, spec, AccessState{}, lifecycle.RotationState{Active: "AKIAACTIVE"})
	if err != nil || string(data["accessKeyId"]) != string(gen) || len(store.generated) != 2 || store.generated[0].id != "AKIAACTIVE" {
		t.Errorf("rotate begin: %v, %q, %v", err, gen, store.generated)
	}

	// Finishing a rotation keeps the active key and nothing else; with none
	// recorded it deletes nothing.
	st := lifecycle.RotationState{Active: "AKIAACTIVE"}
	store.generated = []memKey{{id: "AKIAACTIVE", secret: "s"}, {id: "AKIAPREVIOUS", secret: "s"}}
	if err := e.RotateFinish(ctx, spec, AccessState{}, lifecycle.RotationState{}); err != nil || len(store.generated) != 2 {
		t.Errorf("without an active key: %v, %v", err, store.generated)
	}
	for _, method := range []string{"Keys", "RemoveKey"} {
		store.fail[method] = errStore
		if err := e.RotateFinish(ctx, spec, AccessState{}, st); !errors.Is(err, errStore) {
			t.Errorf("rotate finish, %s: %v", method, err)
		}
		delete(store.fail, method)
	}
	if err := e.RotateFinish(ctx, spec, AccessState{}, st); err != nil || len(store.generated) != 1 || store.generated[0].id != "AKIAACTIVE" {
		t.Errorf("rotate finish: %v, %v", err, store.generated)
	}
}

// A crash between writing the binding Secret and recording the new key leaves
// the Secret holding a key that status does not name. The retry reuses it, and
// deletes neither it nor makes a third key, even with IAM's two keys present.
func TestAWSRotateBeginAfterACrash(t *testing.T) {
	ctx := context.Background()
	spec := s3v1alpha1.BucketAccessSpec{}
	st := lifecycle.RotationState{Active: "AKIAACTIVE"}
	controller := true
	holding := func(id, secret string) func(context.Context) (*corev1.Secret, error) {
		return func(context.Context) (*corev1.Secret, error) {
			return &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{{UID: "uid-1", Controller: &controller}}},
				Data:       map[string][]byte{"accessKeyId": []byte(id), "secretAccessKey": []byte(secret)},
			}, nil
		}
	}

	// Two keys, the binding holds the new one: it is reused.
	e, store := awsExt(t)
	store.generated = []memKey{{id: "AKIAACTIVE", secret: "old"}, {id: "AKIANEW", secret: "new"}}
	e.binding = holding("AKIANEW", "new")
	data, gen, err := e.RotateBegin(ctx, spec, AccessState{}, st)
	if err != nil || gen != "AKIANEW" || string(data["accessKeyId"]) != "AKIANEW" || string(data["secretAccessKey"]) != "new" {
		t.Fatalf("reuse: %v, %q, %v", err, gen, data)
	}
	if len(store.generated) != 2 || store.issued != 0 {
		t.Errorf("a key was made or deleted: %v", store.generated)
	}

	// A third, stray key goes; the active and the binding's stay.
	e, store = awsExt(t)
	store.generated = []memKey{{id: "AKIAACTIVE", secret: "old"}, {id: "AKIANEW", secret: "new"}, {id: "AKIASTRAY", secret: "x"}}
	e.binding = holding("AKIANEW", "new")
	if _, gen, err := e.RotateBegin(ctx, spec, AccessState{}, st); err != nil || gen != "AKIANEW" || len(store.generated) != 2 ||
		store.generated[0].id != "AKIAACTIVE" || store.generated[1].id != "AKIANEW" {
		t.Errorf("stray: %v, %q, %v", err, gen, store.generated)
	}

	// The binding holds a key that is gone, or has no secret, or is the active
	// one: nothing is reused, the leftovers go and a new key is made.
	for name, bind := range map[string]func(context.Context) (*corev1.Secret, error){
		"gone":      holding("AKIAGONE", "s"),
		"no secret": holding("AKIANEW", ""),
		"active":    holding("AKIAACTIVE", "old"),
		"no Secret": func(context.Context) (*corev1.Secret, error) { return nil, nil },
		"foreign": func(context.Context) (*corev1.Secret, error) {
			return &corev1.Secret{Data: map[string][]byte{"accessKeyId": []byte("AKIANEW"), "secretAccessKey": []byte("new")}}, nil
		},
	} {
		e, store = awsExt(t)
		store.generated = []memKey{{id: "AKIAACTIVE", secret: "old"}, {id: "AKIANEW", secret: "new"}}
		e.binding = bind
		_, gen, err := e.RotateBegin(ctx, spec, AccessState{}, st)
		if err != nil || gen == "AKIANEW" || gen == "" || len(store.generated) != 2 || store.generated[0].id != "AKIAACTIVE" {
			t.Errorf("%s: %v, %q, %v", name, err, gen, store.generated)
		}
	}

	// The binding holds the previous key, still in its grace period: it is not
	// the new one, and it is not deleted.
	e, store = awsExt(t)
	store.generated = []memKey{{id: "AKIAACTIVE", secret: "old"}, {id: "AKIAPREV", secret: "p"}}
	e.claim.SetRotationStatus(common.RotationStatus{Active: "AKIAACTIVE", Previous: "AKIAPREV"})
	e.binding = holding("AKIAPREV", "p")
	if _, gen, err := e.RotateBegin(ctx, spec, AccessState{}, st); err != nil || gen == "AKIAPREV" || gen == "" {
		t.Errorf("previous: %v, %q", err, gen)
	}

	// No active key is recorded: the Secret's key is not taken for a new one.
	e, store = awsExt(t)
	store.generated = []memKey{{id: "AKIANEW", secret: "new"}}
	e.binding = holding("AKIANEW", "new")
	if _, gen, err := e.RotateBegin(ctx, spec, AccessState{}, lifecycle.RotationState{}); err != nil || gen == "AKIANEW" || gen == "" {
		t.Errorf("no active: %v, %q", err, gen)
	}

	// The Secret cannot be read, or the keys cannot be listed or deleted.
	e, store = awsExt(t)
	e.binding = func(context.Context) (*corev1.Secret, error) { return nil, errStore }
	if _, _, err := e.RotateBegin(ctx, spec, AccessState{}, st); !errors.Is(err, errStore) {
		t.Errorf("binding fails: %v", err)
	}
	for _, method := range []string{"Keys", "RemoveKey"} {
		e, store = awsExt(t)
		store.generated = []memKey{{id: "AKIAACTIVE", secret: "old"}, {id: "AKIANEW", secret: "new"}, {id: "AKIASTRAY", secret: "x"}}
		e.binding = holding("AKIANEW", "new")
		store.fail[method] = errStore
		if _, _, err := e.RotateBegin(ctx, spec, AccessState{}, st); !errors.Is(err, errStore) {
			t.Errorf("reuse, %s: %v", method, err)
		}
	}
}

// Issuing a key again (the active one is gone) keeps the previous generation,
// still in its grace period, and drops only the keys that are neither.
func TestAWSIssueKeepsThePreviousKey(t *testing.T) {
	ctx := context.Background()
	e, store := awsExt(t)
	store.generated = []memKey{{id: "AKIAPREVIOUS", secret: "p"}, {id: "AKIASTRAY", secret: "x"}}
	e.claim.SetRotationStatus(common.RotationStatus{Active: "AKIAGONE", Previous: "AKIAPREVIOUS"})
	data, err := e.issue(ctx, "AKIAGONE")
	if err != nil {
		t.Fatal(err)
	}
	rs := e.claim.GetRotationStatus()
	if len(store.generated) != 2 || store.generated[0].id != "AKIAPREVIOUS" || rs.Previous != "AKIAPREVIOUS" ||
		rs.Active == "" || rs.Active == "AKIAGONE" || string(data["accessKeyId"]) != rs.Active {
		t.Errorf("rotation %+v, keys %v", rs, store.generated)
	}
}
