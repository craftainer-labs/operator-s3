package controller

import (
	"context"
	"errors"
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/craftainer-labs/lib-operator-go/pkg/binding"
	"github.com/craftainer-labs/lib-operator-go/pkg/lifecycle"

	s3v1alpha1 "github.com/craftainer-labs/operator-s3/api/v1alpha1"
	"github.com/craftainer-labs/operator-s3/internal/s3client"
)

// ErrExistsUnmanaged means a bucket with the claim's name exists and nothing
// shows that this claim created it: it has no ownership marker and the claim's
// status does not record a create. The operator never adopts or deletes it.
// The README says how to resolve it.
var ErrExistsUnmanaged = errors.New("bucket exists and is not managed by this operator")

// ErrInvalidLifecycle means two lifecycle rules of a Bucket share an id.
var ErrInvalidLifecycle = errors.New("lifecycle rule ids must be unique")

// createIssued is written to status.externalId before the bucket is created.
// It records that this claim issued the create, so a bucket that has no
// marker because the operator stopped between the create and the tag write
// (two requests that cannot be one transaction) is taken over, and one that
// someone else made is not.
const createIssued = "create-issued"

// BucketReconciler reconciles Bucket claims (a data kind).
type BucketReconciler struct{ Config }

// Reconcile implements reconcile.Reconciler.
func (r *BucketReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var b s3v1alpha1.Bucket
	if err := r.Client.Get(ctx, req.NamespacedName, &b); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// A claim that is being deleted and never issued a create has nothing on the
	// store: it is released without contacting the store, which may be gone.
	if !b.DeletionTimestamp.IsZero() && b.GetExternalID() == "" {
		return lifecycle.Reconcile[s3v1alpha1.BucketSpec, s3client.BucketState](ctx, r.Client, &b, b.Spec,
			nothingMade[s3v1alpha1.BucketSpec, s3client.BucketState]{}, r.options(nil))
	}
	be, err := r.backend(ctx, b.Namespace, b.Spec.ServerRef)
	opts := r.options(err)
	var ext lifecycle.External[s3v1alpha1.BucketSpec, s3client.BucketState]
	if err == nil {
		opts.ServerHost = be.host
		x := newBucketExternal(be, &b)
		x.persist = func(ctx context.Context) error { return r.Client.Status().Update(ctx, &b) }
		ext = x
	}
	return lifecycle.Reconcile(ctx, r.Client, &b, b.Spec, ext, opts)
}

// options are the lifecycle options of the controller; resolveErr is the error
// of resolving the store, or nil.
func (r *BucketReconciler) options(resolveErr error) lifecycle.Options {
	return lifecycle.Options{
		Class: lifecycle.ClassData, ManagedBy: r.ManagedBy, Recorder: r.Recorder, APIReader: r.APIReader,
		DefaultDeletionPolicy: r.DefaultDeletionPolicy, ResolveErr: resolveErr, Now: r.Now,
	}
}

// SetupWithManager registers the controller.
func (r *BucketReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&s3v1alpha1.Bucket{}).
		WithEventFilter(lifecycle.Predicates()).
		Named("bucket").
		Complete(r)
}

// bucketExternal is the lifecycle.External of one Bucket.
type bucketExternal struct {
	be     *backend
	claim  *s3v1alpha1.Bucket
	name   string
	marker string
	// persist writes the claim's status now, instead of at the end of the
	// reconcile.
	persist func(context.Context) error
}

func newBucketExternal(be *backend, claim *s3v1alpha1.Bucket) *bucketExternal {
	// Observe goes by status.externalName, never by re-deriving (ADR 0002,
	// section 3); the name is derived once, at the first reconcile.
	name := claim.GetExternalName()
	if name == "" {
		name = s3client.BucketName(claim.Namespace, claim.Name)
		claim.SetExternalName(name)
	}
	return &bucketExternal{be: be, claim: claim, name: name, marker: claimID(claim)}
}

// params turns the spec into what the store is asked for. Object lock needs
// versioning, so it implies it.
func (e *bucketExternal) params(spec s3v1alpha1.BucketSpec) (s3client.BucketParams, error) {
	p := s3client.BucketParams{
		Name: e.name, Marker: e.marker, Versioning: spec.Versioning || spec.ObjectLock, ObjectLock: spec.ObjectLock,
	}
	if spec.Quota != nil {
		p.Quota = spec.Quota.HardBytes.Value()
	}
	seen := map[string]bool{}
	for _, r := range spec.LifecycleRules {
		if seen[r.ID] {
			return p, fmt.Errorf("%w: %q", ErrInvalidLifecycle, r.ID)
		}
		seen[r.ID] = true
		p.Rules = append(p.Rules, s3client.Rule{ID: r.ID, Prefix: r.Prefix, Days: r.ExpirationDays})
	}
	return p, nil
}

// Observe implements lifecycle.External. A bucket whose marker names another
// claim is refused. One with no marker is taken over by Update only when the
// claim's status records that it issued the create (a Create that stopped
// before it stamped the marker); otherwise it is refused as unmanaged.
//
// While the claim is being deleted a bucket that is not its own is not refused
// but left out (it does not exist as far as the claim is concerned), so the
// claim is released and the bucket is never touched.
func (e *bucketExternal) Observe(ctx context.Context, spec s3v1alpha1.BucketSpec) (lifecycle.Observation[s3client.BucketState], error) {
	none := lifecycle.Observation[s3client.BucketState]{}
	// Reconcile adds the finalizer with a patch that replaces the claim with the
	// server's copy, which loses a name set before it; set it again here.
	e.claim.SetExternalName(e.name)
	st, err := e.be.store.Bucket(ctx, e.name)
	if errors.Is(err, s3client.ErrBucketExists) {
		// The name belongs to another account: not ours, never touched.
		return none, e.foreign(fmt.Errorf("%w: %w: %q", lifecycle.ErrNameCollision, ErrExistsUnmanaged, e.name))
	}
	if err != nil || !st.Exists {
		return none, err
	}
	if st.Marker() != "" && st.Marker() != e.marker {
		return none, e.foreign(fmt.Errorf("%w: bucket %q belongs to another claim", lifecycle.ErrNameCollision, e.name))
	}
	if st.Marker() == "" && e.claim.GetExternalID() != createIssued {
		return none, e.foreign(fmt.Errorf("%w: %w: %q", lifecycle.ErrNameCollision, ErrExistsUnmanaged, e.name))
	}
	// A claim from before the create was recorded: the marker is the proof that
	// the bucket is ours, so the record is made now (written with the status).
	if st.Marker() == e.marker && e.claim.GetExternalID() == "" {
		e.claim.SetExternalID(createIssued)
	}
	p, err := e.params(spec)
	if err != nil {
		return none, err
	}
	upToDate := st.Marker() == e.marker && st.Versioning == p.Versioning && st.Quota == p.Quota &&
		s3client.RulesEqual(st.Rules, p.Rules)
	return lifecycle.Observation[s3client.BucketState]{Exists: true, UpToDate: upToDate, Current: st}, nil
}

// foreign says what to do with a refusal of a bucket that is not this claim's:
// a claim that is being deleted leaves the bucket alone and goes on (nil), any
// other keeps the refusal.
func (e *bucketExternal) foreign(refusal error) error {
	if e.claim.DeletionTimestamp.IsZero() {
		return refusal
	}
	return nil
}

// Create implements lifecycle.External; a data kind has no binding data.
func (e *bucketExternal) Create(ctx context.Context, spec s3v1alpha1.BucketSpec) (binding.Data, error) {
	p, err := e.params(spec)
	if err != nil {
		return nil, err
	}
	// Record the intent before the create, so a crash after it is recognised.
	e.claim.SetExternalID(createIssued)
	if err := e.persist(ctx); err != nil {
		return nil, err
	}
	err = e.be.store.CreateBucket(ctx, p)
	if !errors.Is(err, s3client.ErrBucketExists) {
		return nil, err
	}
	// Somebody made the bucket between Observe and this create (RustFS would
	// have answered the create with success). It is not ours, and the record
	// above must not let the next reconcile take it over.
	e.claim.SetExternalID("")
	if err := e.persist(ctx); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("%w: %w: %q", lifecycle.ErrNameCollision, ErrExistsUnmanaged, e.name)
}

// Update implements lifecycle.External.
func (e *bucketExternal) Update(ctx context.Context, spec s3v1alpha1.BucketSpec, cur s3client.BucketState) (binding.Data, error) {
	p, err := e.params(spec)
	if err != nil {
		return nil, err
	}
	return nil, e.be.store.UpdateBucket(ctx, p, cur)
}

// Delete implements lifecycle.External. Observe has refused a bucket this
// claim did not create, so only its own bucket gets here; one that holds
// objects is refused by the store and stays until it is emptied.
func (e *bucketExternal) Delete(ctx context.Context, _ s3client.BucketState) error {
	return e.be.store.DeleteBucket(ctx, e.name)
}
