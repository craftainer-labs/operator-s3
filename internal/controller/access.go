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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/craftainer-labs/lib-operator-go/pkg/binding"
	"github.com/craftainer-labs/lib-operator-go/pkg/lifecycle"

	s3v1alpha1 "github.com/craftainer-labs/operator-s3/api/v1alpha1"
	"github.com/craftainer-labs/operator-s3/internal/s3client"
)

var (
	// ErrBucketNotUsable means the Bucket a BucketAccess names is missing, has
	// no bucket yet or lives on another store.
	ErrBucketNotUsable = errors.New("the referenced Bucket cannot be used")
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
	be, err := r.backend(ctx, a.Namespace, a.Spec.ServerRef)
	opts := lifecycle.Options{
		Class: lifecycle.ClassCredential, BindingType: binding.TypeS3, Provider: binding.DefaultProvider,
		ManagedBy: r.ManagedBy, Recorder: r.Recorder, APIReader: r.APIReader, ResolveErr: err, Now: r.Now,
	}
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
		ext = x
	}
	return lifecycle.Reconcile(ctx, r.Client, &a, a.Spec, ext, opts)
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
	if b.GetExternalName() == "" {
		return "", fmt.Errorf("%w: Bucket %q has no bucket yet", ErrBucketNotUsable, key.Name)
	}
	return b.GetExternalName(), nil
}

// SetupWithManager registers the controller; it also watches the binding
// Secrets it owns.
func (r *AccessReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&s3v1alpha1.BucketAccess{}).
		Owns(&corev1.Secret{}).
		WithEventFilter(lifecycle.Predicates()).
		Named("bucketaccess").
		Complete(r)
}

// accessExternal is the lifecycle.Rotator of one BucketAccess (ADR 0002,
// section 8): an IAM user with the bucket policy, and one service account
// per generation. A service account inherits the policy of its user, and its
// access key is derived from the claim's UID and the rotation sequence, so a
// retry after a crash reissues the same key.
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
}

func newAccessExternal(be *backend, claim *s3v1alpha1.BucketAccess) *accessExternal {
	user := claim.GetExternalName()
	if user == "" {
		user = s3client.UserName(claim.Namespace, claim.Name)
		claim.SetExternalName(user)
	}
	return &accessExternal{be: be, claim: claim, user: user}
}

// key is the access key of generation seq.
func (e *accessExternal) key(seq int64) string { return s3client.AccessKey(string(e.claim.UID), seq) }

// activeKey is the access key of the generation the binding Secret holds.
func (e *accessExternal) activeKey() string { return e.key(e.claim.GetRotationStatus().Sequence) }

// policy builds the access policy and its name, refusing a prefix that would
// widen it.
func (e *accessExternal) policy(spec s3v1alpha1.BucketAccessSpec) (string, []byte, error) {
	if !objectKeyOK(spec.Prefix) {
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
	return s3client.AccessParams{User: e.user, Secret: newSecret(), Policy: name, PolicyDoc: doc}, nil
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
	if err != nil || !us.Exists {
		return none, err
	}
	if e.claim.GetExternalID() == "" {
		return none, fmt.Errorf("%w: %w: %q", lifecycle.ErrNameCollision, ErrExistsUnmanaged, e.user)
	}
	key := e.activeKey()
	exists, err := e.be.store.KeyExists(ctx, key)
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

// issue sets the secret key of the service account key and returns the
// binding data for it.
func (e *accessExternal) issue(ctx context.Context, key string) (binding.Data, error) {
	secret := newSecret()
	if err := e.be.store.IssueKey(ctx, e.user, key, secret); err != nil {
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

// Update implements lifecycle.External. It also sets the secret key of the
// active service account again, because the binding Secret has to be written
// with a value the store cannot be asked for.
func (e *accessExternal) Update(ctx context.Context, spec s3v1alpha1.BucketAccessSpec, cur AccessState) (binding.Data, error) {
	p, err := e.params(spec)
	if err != nil {
		return nil, err
	}
	if err := e.be.store.ApplyAccess(ctx, p, cur.User); err != nil {
		return nil, err
	}
	return e.issue(ctx, cur.Key)
}

// Delete implements lifecycle.External: every generation goes with the claim.
func (e *accessExternal) Delete(ctx context.Context, cur AccessState) error {
	return e.be.store.DeleteAccess(ctx, e.user, cur.User.Policies)
}

// RotateBegin implements lifecycle.Rotator: the next generation's service
// account. Its key follows from the sequence, so it is the same on a retry.
func (e *accessExternal) RotateBegin(ctx context.Context, _ s3v1alpha1.BucketAccessSpec, _ AccessState, st lifecycle.RotationState) (binding.Data, lifecycle.Generation, error) {
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
	if st.Sequence < 1 {
		return nil
	}
	return e.be.store.DeleteKey(ctx, e.key(st.Sequence-1))
}

// data is the s3 binding of one service account (ADR 0002, section 7). The CA
// is the store's bundle.
func (e *accessExternal) data(key, secret string) binding.Data {
	pathStyle := e.be.spec.PathStyle == nil || *e.be.spec.PathStyle
	return binding.Data{
		"endpoint":        []byte(e.be.conn.Endpoint),
		"region":          []byte(e.be.region()),
		"bucket":          []byte(e.bucket),
		"accessKeyId":     []byte(key),
		"secretAccessKey": []byte(secret),
		"prefix":          []byte(e.claim.Spec.Prefix),
		"pathStyle":       []byte(strconv.FormatBool(pathStyle)),
		"sslrootcert":     e.be.conn.CABundle,
	}
}

var _ lifecycle.Rotator[s3v1alpha1.BucketAccessSpec, AccessState] = (*accessExternal)(nil)
