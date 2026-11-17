package controller

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/craftainer-labs/lib-operator-go/pkg/binding"
	"github.com/craftainer-labs/lib-operator-go/pkg/conditions"
	"github.com/craftainer-labs/lib-operator-go/pkg/lifecycle"

	s3v1alpha1 "github.com/craftainer-labs/operator-s3/api/v1alpha1"
	"github.com/craftainer-labs/operator-s3/internal/s3client"
)

var (
	// ErrBucketNotUsable means the Bucket a BucketAccess names is missing, has
	// no bucket yet or lives on another store.
	ErrBucketNotUsable = errors.New("the referenced Bucket cannot be used")
	// ErrBindingNotUsable means the binding Secret does not hold the secret key
	// of the active service account, so the access cannot be updated in place;
	// the next reconcile rotates it.
	ErrBindingNotUsable = errors.New("the binding Secret does not hold the active key; the next reconcile rotates it")
	// ErrInvalidPrefix means an access prefix holds a wildcard or a policy
	// variable, which would widen the policy built from it.
	ErrInvalidPrefix = errors.New("prefix must not contain *, ? or $")
)

// AccessState is what Observe reads about the IAM side of a BucketAccess: the
// user, and whether the service account of the active generation exists.
type AccessState struct {
	User      s3client.UserState
	Key       string
	KeyExists bool
}

// AccessReconciler reconciles BucketAccess claims (a credential kind).
type AccessReconciler struct{ Config }

// Reconcile implements reconcile.Reconciler.
func (r *AccessReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var a s3v1alpha1.BucketAccess
	if err := r.Client.Get(ctx, req.NamespacedName, &a); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// A claim that is being deleted and never issued a create has nothing on the
	// store (an IAM user of its name, if there is one, is not its own): it is
	// released without contacting the store, which may be gone.
	if !a.DeletionTimestamp.IsZero() && a.GetExternalID() == "" {
		return lifecycle.Reconcile[s3v1alpha1.BucketAccessSpec, AccessState](ctx, r.Client, &a, a.Spec,
			nothingMade[s3v1alpha1.BucketAccessSpec, AccessState]{}, r.options(nil))
	}
	be, err := r.backend(ctx, a.Namespace, a.Spec.ServerRef)
	opts := r.options(err)
	var ext lifecycle.External[s3v1alpha1.BucketAccessSpec, AccessState]
	if err == nil {
		opts.ServerHost = be.host
		x := newAccessExternal(be, &a)
		// The bucket is the one the Bucket claim names in its status, not one
		// derived again here. Deleting an access does not need the Bucket (it
		// may be gone first), so a failure only counts when not deleting.
		var berr error
		x.bucket, berr = r.checkBucket(ctx, &a)
		if a.DeletionTimestamp.IsZero() {
			x.pre = berr
		}
		x.persist = func(ctx context.Context) error { return r.Client.Status().Update(ctx, &a) }
		x.binding = func(ctx context.Context) (*corev1.Secret, error) { return r.bindingSecret(ctx, &a) }
		x.drop = func(ctx context.Context, s *corev1.Secret) error {
			return client.IgnoreNotFound(r.Client.Delete(ctx, s, client.Preconditions{UID: &s.UID}))
		}
		ext = x
	}
	return lifecycle.Reconcile(ctx, r.Client, &a, a.Spec, ext, opts)
}

// options are the lifecycle options of the controller; resolveErr is the error
// of resolving the store, or nil.
func (r *AccessReconciler) options(resolveErr error) lifecycle.Options {
	return lifecycle.Options{
		Class: lifecycle.ClassCredential, BindingType: binding.TypeS3, Provider: binding.DefaultProvider,
		ManagedBy: r.ManagedBy, Recorder: r.Recorder, APIReader: r.APIReader, ResolveErr: resolveErr, Now: r.Now,
	}
}

// checkBucket makes sure the Bucket exists in the claim's namespace on the same
// store: an access is for one bucket of the store it names. It returns the
// Bucket's status.externalName.
func (r *AccessReconciler) checkBucket(ctx context.Context, a *s3v1alpha1.BucketAccess) (string, error) {
	var b s3v1alpha1.Bucket
	key := client.ObjectKey{Namespace: a.Namespace, Name: a.Spec.BucketRef.Name}
	if err := r.Client.Get(ctx, key, &b); err != nil {
		if apierrors.IsNotFound(err) {
			return "", fmt.Errorf("%w: Bucket %q not found", ErrBucketNotUsable, key.Name)
		}
		return "", err
	}
	if b.Spec.ServerRef != a.Spec.ServerRef {
		return "", fmt.Errorf("%w: Bucket %q is on %s %q, not on %s %q", ErrBucketNotUsable, key.Name,
			b.Spec.ServerRef.Kind, b.Spec.ServerRef.Name, a.Spec.ServerRef.Kind, a.Spec.ServerRef.Name)
	}
	if !b.DeletionTimestamp.IsZero() {
		return "", fmt.Errorf("%w: Bucket %q is being deleted", ErrBucketNotUsable, key.Name)
	}
	if b.GetExternalName() == "" {
		return "", fmt.Errorf("%w: Bucket %q has no bucket yet", ErrBucketNotUsable, key.Name)
	}
	// The name is recorded before the bucket exists, so only Ready says it does.
	if !meta.IsStatusConditionTrue(b.Status.Conditions, conditions.Ready) {
		return "", fmt.Errorf("%w: Bucket %q is not Ready", ErrBucketNotUsable, key.Name)
	}
	return b.GetExternalName(), nil
}

// bindingSecret is the claim's binding Secret, nil when there is none. It reads
// through the API reader, because the cache only holds Secrets with the
// operator's label.
func (r *AccessReconciler) bindingSecret(ctx context.Context, a *s3v1alpha1.BucketAccess) (*corev1.Secret, error) {
	var s corev1.Secret
	err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: a.Namespace, Name: a.GetBindingSecretName()}, &s)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	return &s, err
}

// bucketRefIndex indexes BucketAccesses by the name of the Bucket they use.
const bucketRefIndex = ".spec.bucketRef.name"

func indexBucketRef(o client.Object) []string {
	return []string{o.(*s3v1alpha1.BucketAccess).Spec.BucketRef.Name}
}

// accessesOfBucket maps a Bucket to the BucketAccesses that name it, so an
// access that waits for its Bucket is reconciled when the Bucket changes.
func accessesOfBucket(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, o client.Object) []reconcile.Request {
		var list s3v1alpha1.BucketAccessList
		if err := c.List(ctx, &list, client.InNamespace(o.GetNamespace()), client.MatchingFields{bucketRefIndex: o.GetName()}); err != nil {
			log.FromContext(ctx).Error(err, "cannot list the accesses of a Bucket", "bucket", o.GetName())
			return nil
		}
		reqs := make([]reconcile.Request, len(list.Items))
		for i := range list.Items {
			reqs[i] = reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])}
		}
		return reqs
	}
}

// SetupWithManager registers the controller. It also watches the binding
// Secrets it owns (every change, so an edited Secret is repaired) and the
// Buckets its claims name (so an access that waits for its Bucket goes on when
// the Bucket is Ready). Only the claims themselves pass the lifecycle filter.
func (r *AccessReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &s3v1alpha1.BucketAccess{}, bucketRefIndex, indexBucketRef); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&s3v1alpha1.BucketAccess{}, builder.WithPredicates(lifecycle.Predicates())).
		Owns(&corev1.Secret{}).
		Watches(&s3v1alpha1.Bucket{}, handler.EnqueueRequestsFromMapFunc(accessesOfBucket(mgr.GetClient()))).
		Named("bucketaccess").
		Complete(r)
}

// accessExternal is the lifecycle.Rotator of one BucketAccess (ADR 0002,
// section 8): an IAM user with the bucket policy, and one access key per
// generation.
//
// On RustFS a key is a service account of the user. It inherits the policy of
// its user, and its key id is derived from the claim's UID and the rotation
// sequence, so a retry after a crash reissues the same key.
//
// On AWS (be.gen is set) the key is an IAM access key. IAM generates the key id
// and shows the secret once, so nothing can be derived and a key whose answer
// was lost cannot be recovered. status.rotation.active holds the active key id.
// IAM allows two keys per user, so every issue first deletes the keys that are
// not the active one: a key left by a crashed rotation, or one whose secret is
// lost, is never a reason to hit the limit.
type accessExternal struct {
	be     *backend
	claim  *s3v1alpha1.BucketAccess
	user   string
	bucket string
	// pre is a precondition failure (the Bucket is not usable). Observe
	// reports it, and Reconcile calls nothing else when Observe fails.
	pre error
	// persist writes the claim's status now, instead of at the end of the
	// reconcile.
	persist func(context.Context) error
	// binding reads the binding Secret as it is now; nil when there is none.
	binding func(context.Context) (*corev1.Secret, error)
	// drop deletes a binding Secret.
	drop func(context.Context, *corev1.Secret) error
}

func newAccessExternal(be *backend, claim *s3v1alpha1.BucketAccess) *accessExternal {
	user := claim.GetExternalName()
	if user == "" {
		user = s3client.UserName(claim.Namespace, claim.Name)
		if be.gen != nil {
			user = s3client.IAMUserName(claim.Namespace, claim.Name)
		}
		claim.SetExternalName(user)
	}
	return &accessExternal{be: be, claim: claim, user: user}
}

// key is the access key of generation seq.
func (e *accessExternal) key(seq int64) string { return s3client.AccessKey(string(e.claim.UID), seq) }

// activeKey is the access key of the generation the binding Secret holds: the
// one derived from the sequence on RustFS, the one status.rotation.active
// records on AWS ("" before the first is issued).
func (e *accessExternal) activeKey() string {
	rs := e.claim.GetRotationStatus()
	if e.be.gen != nil {
		return rs.Active
	}
	return e.key(rs.Sequence)
}

// keyExists reports whether the active key is on the store.
func (e *accessExternal) keyExists(ctx context.Context, key string) (bool, error) {
	if e.be.gen == nil {
		return e.be.keys.KeyExists(ctx, key)
	}
	ids, err := e.be.gen.Keys(ctx, e.user)
	return key != "" && slices.Contains(ids, key), err
}

// policy builds the access policy and its name, refusing a prefix that would
// widen it.
func (e *accessExternal) policy(spec s3v1alpha1.BucketAccessSpec) (string, []byte, error) {
	if !s3v1alpha1.PrefixOK(spec.Prefix) {
		return "", nil, ErrInvalidPrefix
	}
	doc := s3client.AccessPolicy(e.bucket, spec.Prefix, spec.Access != s3v1alpha1.AccessReadOnly)
	return s3client.PolicyName(e.user, doc), doc, nil
}

func (e *accessExternal) params(spec s3v1alpha1.BucketAccessSpec) (s3client.AccessParams, error) {
	name, doc, err := e.policy(spec)
	if err != nil {
		return s3client.AccessParams{}, err
	}
	// The user's own secret key is never handed out: the keys of the binding
	// are service accounts, and a key nobody holds cannot be used to log in.
	return s3client.AccessParams{User: e.user, Secret: newSecret(), Policy: name, PolicyDoc: doc, Marker: claimID(e.claim)}, nil
}

// newSecret is a 40-character secret key, the longest RustFS accepts.
func newSecret() string { return rand.Text() + rand.Text()[:14] }

// Observe implements lifecycle.External. A user that exists while the claim's
// status records no create is not ours: IAM users cannot hold a marker, so
// the status is the only proof, and the operator never adopts or deletes such
// a user. The access is up to date when the user is enabled with exactly the
// current policy attached and the active service account exists.
func (e *accessExternal) Observe(ctx context.Context, spec s3v1alpha1.BucketAccessSpec) (lifecycle.Observation[AccessState], error) {
	none := lifecycle.Observation[AccessState]{}
	// Reconcile adds the finalizer with a patch that replaces the claim with
	// the server's copy, which loses a name set before it; set it again here.
	e.claim.SetExternalName(e.user)
	if e.pre != nil {
		return none, e.pre
	}
	us, err := e.be.store.User(ctx, e.user)
	if err != nil {
		return none, err
	}
	if !us.Exists {
		return e.leftovers(ctx)
	}
	// An IAM user holds the claim as a tag, which proves it is this claim's
	// whatever the status says; its ARN is the claim's external ID.
	owned := e.claim.GetExternalID() != ""
	if e.be.gen != nil {
		owned = us.Marker == claimID(e.claim)
	}
	if !owned {
		return none, fmt.Errorf("%w: %w: %q", lifecycle.ErrNameCollision, ErrExistsUnmanaged, e.user)
	}
	if e.be.gen != nil {
		e.claim.SetExternalID(us.ARN)
	}
	key := e.activeKey()
	exists, err := e.keyExists(ctx, key)
	if err != nil {
		return none, err
	}
	// An invalid prefix gives no policy name, so the access is never up to
	// date and Update reports it; deleting does not need a valid spec.
	name, _, _ := e.policy(spec)
	upToDate := us.Enabled && exists && slices.Equal(us.Policies, []string{name})
	return lifecycle.Observation[AccessState]{
		Exists: true, UpToDate: upToDate, Current: AccessState{User: us, Key: key, KeyExists: exists},
	}, nil
}

// leftovers reports an access whose user is already gone as existing while it
// is being deleted and the store still holds policies it named, so that Delete
// removes them. Otherwise a missing user is a missing access.
func (e *accessExternal) leftovers(ctx context.Context) (lifecycle.Observation[AccessState], error) {
	none := lifecycle.Observation[AccessState]{}
	if e.claim.DeletionTimestamp.IsZero() || e.claim.GetExternalID() == "" {
		return none, nil
	}
	owned, err := e.be.store.OwnedPolicies(ctx, e.user)
	if err != nil || len(owned) == 0 {
		return none, err
	}
	return lifecycle.Observation[AccessState]{Exists: true}, nil
}

// issueGenerated creates an IAM access key, after deleting every key but keep:
// they are leftovers whose secret nobody holds, and IAM allows two keys. It
// returns the binding data and the key id.
func (e *accessExternal) issueGenerated(ctx context.Context, keep string) (binding.Data, string, error) {
	ids, err := e.be.gen.Keys(ctx, e.user)
	if err != nil {
		return nil, "", err
	}
	for _, id := range ids {
		if id == keep {
			continue
		}
		if err := e.be.gen.RemoveKey(ctx, e.user, id); err != nil {
			return nil, "", err
		}
	}
	id, secret, err := e.be.gen.NewKey(ctx, e.user)
	if err != nil {
		return nil, "", err
	}
	return e.data(id, secret), id, nil
}

// issue sets the secret key of the service account key and returns the
// binding data for it; on AWS it creates a new key (the one named is only the
// RustFS key to derive) and records it as the active one.
func (e *accessExternal) issue(ctx context.Context, key string) (binding.Data, error) {
	if e.be.gen != nil {
		// The previous generation, still in its grace period, is kept, so a
		// listing that has not caught up with a rotation never cuts it off.
		rs := e.claim.GetRotationStatus()
		data, id, err := e.issueGenerated(ctx, rs.Previous)
		if err != nil {
			return nil, err
		}
		rs.Active = id
		e.claim.SetRotationStatus(rs)
		return data, nil
	}
	secret := newSecret()
	if err := e.be.keys.IssueKey(ctx, e.user, key, secret); err != nil {
		return nil, err
	}
	e.claim.SetExternalID(key)
	return e.data(key, secret), nil
}

// Create implements lifecycle.External: the user with its policy, and the
// first generation's service account.
func (e *accessExternal) Create(ctx context.Context, spec s3v1alpha1.BucketAccessSpec) (binding.Data, error) {
	p, err := e.params(spec)
	if err != nil {
		return nil, err
	}
	// Record the intent before the create, so a crash after it is recognised.
	e.claim.SetExternalID(createIssued)
	if err := e.persist(ctx); err != nil {
		return nil, err
	}
	if err := e.be.store.ApplyAccess(ctx, p, s3client.UserState{}); err != nil {
		return nil, err
	}
	return e.issue(ctx, e.activeKey())
}

// Update implements lifecycle.External: the user and its policy follow the
// spec. The active service account keeps its secret key, so a change of policy
// does not invalidate the credentials in use; the binding Secret is returned
// as it is, with the fields that follow the spec.
//
// A key in use is never reset in place. When the binding Secret does not hold
// it (it is missing, or holds another key or no secret), Update has applied the
// policy and returns ErrBindingNotUsable, after deleting a Secret of this claim
// that holds something else: the next reconcile finds the Secret missing and
// the lifecycle rotates (next sequence, grace period). A key that is gone from
// the store has nothing to reset, so it is issued again.
func (e *accessExternal) Update(ctx context.Context, spec s3v1alpha1.BucketAccessSpec, cur AccessState) (binding.Data, error) {
	p, err := e.params(spec)
	if err != nil {
		return nil, err
	}
	if err := e.be.store.ApplyAccess(ctx, p, cur.User); err != nil {
		return nil, err
	}
	if !cur.KeyExists {
		return e.issue(ctx, cur.Key)
	}
	s, err := e.binding(ctx)
	if err != nil {
		return nil, err
	}
	if s != nil && len(s.Data["secretAccessKey"]) > 0 && string(s.Data["accessKeyId"]) == cur.Key {
		return e.data(cur.Key, string(s.Data["secretAccessKey"])), nil
	}
	if s != nil && metav1.IsControlledBy(s, e.claim) {
		if err := e.drop(ctx, s); err != nil {
			return nil, err
		}
	}
	return nil, ErrBindingNotUsable
}

// Delete implements lifecycle.External: every generation goes with the claim.
// The service accounts are the ones this claim derived (the sequences up to the
// next one, which a rotation may have issued before it failed); any other
// account under the user name is left alone. The policies are found in the
// store by their name, whether or not the user still has them.
func (e *accessExternal) Delete(ctx context.Context, _ AccessState) error {
	last := e.claim.GetRotationStatus().Sequence + 1
	keys := make([]string, 0, last+1)
	for seq := int64(0); seq <= last; seq++ {
		keys = append(keys, e.key(seq))
	}
	return e.be.store.DeleteAccess(ctx, e.user, keys)
}

// RotateBegin implements lifecycle.Rotator: the next generation's service
// account. Its key follows from the sequence, so it is the same on a retry.
func (e *accessExternal) RotateBegin(ctx context.Context, _ s3v1alpha1.BucketAccessSpec, _ AccessState, st lifecycle.RotationState) (binding.Data, lifecycle.Generation, error) {
	if e.be.gen != nil {
		// The second key of the user. Every other key is a leftover of a
		// crashed rotation (its secret was never written to the binding
		// Secret) and goes first, so a retry never hits IAM's limit of two.
		data, id, err := e.issueGenerated(ctx, string(st.Active))
		return data, lifecycle.Generation(id), err
	}
	next := e.key(st.Sequence + 1)
	data, err := e.issue(ctx, next)
	if err != nil {
		return nil, "", err
	}
	return data, lifecycle.Generation(next), nil
}

// RotateFinish implements lifecycle.Rotator. After a rotation the sequence is
// the one of the active generation, and the previous one is the sequence
// before it. Deleting a key that is gone succeeds.
func (e *accessExternal) RotateFinish(ctx context.Context, _ s3v1alpha1.BucketAccessSpec, _ AccessState, st lifecycle.RotationState) error {
	if e.be.gen != nil {
		return e.retireOthers(ctx, string(st.Active))
	}
	if st.Sequence < 1 {
		return nil
	}
	return e.be.keys.DeleteKey(ctx, e.key(st.Sequence-1))
}

// retireOthers deletes every key of the user but the active one: the previous
// generation, and any a crashed rotation left. With no active key recorded it
// deletes nothing.
func (e *accessExternal) retireOthers(ctx context.Context, active string) error {
	if active == "" {
		return nil
	}
	ids, err := e.be.gen.Keys(ctx, e.user)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if id != active {
			if err := e.be.gen.RemoveKey(ctx, e.user, id); err != nil {
				return err
			}
		}
	}
	return nil
}

// data is the s3 binding of one service account (ADR 0002, section 7). The CA
// is the store's bundle.
func (e *accessExternal) data(key, secret string) binding.Data {
	pathStyle := e.be.spec.PathStyleOf()
	d := binding.Data{
		"endpoint":        []byte(e.be.conn.Endpoint),
		"region":          []byte(e.be.region()),
		"bucket":          []byte(e.bucket),
		"accessKeyId":     []byte(key),
		"secretAccessKey": []byte(secret),
		"prefix":          []byte(e.claim.Spec.Prefix),
		"pathStyle":       []byte(strconv.FormatBool(pathStyle)),
		"sslrootcert":     e.be.conn.CABundle,
	}
	if e.be.gen != nil {
		d["cloud"] = []byte("aws")
	}
	return d
}

var _ lifecycle.Rotator[s3v1alpha1.BucketAccessSpec, AccessState] = (*accessExternal)(nil)
