// Package gcs is the object store backend for a Google Cloud project (NKs3-2a):
// the buckets of a store whose provider is GCP, through the Cloud Storage JSON
// API (cloud.google.com/go/storage). The HTTP client (the credentials, the
// egress policy, the proxy and the emulator endpoint of the tests) comes from
// pkg/cloud's Egress.GCP, so every request dials through the egress policy.
//
// A Store has the method set of controller.Store, but only the bucket calls
// are implemented. BucketAccess on GCP awaits the user's API decision (the
// ADR 0004 addendum proposes it): ApplyAccess answers ErrAccessNotImplemented,
// and the other access methods find nothing, so a claim that never got an
// account can still be deleted.
//
// The bucket records its claim in a label (labelMarker): a GCS label key
// allows lowercase letters, digits, underscore and dash only, and a value
// the same up to 63 characters, so the marker "<namespace>/<name>/<uid>" is
// written as a digest (EncodeMarker). The bucket is created with its label,
// versioning and lifecycle in one request, so there is no unmarked bucket to
// adopt. A quota has no GCS equivalent and object lock none that the Bucket
// field means (GCS retention is a different feature), so a Store refuses
// either (ErrQuotaUnsupported, ErrObjectLockUnsupported). GCS lifecycle
// rules have no ID: they are read and compared by prefix and days
// (s3client.BucketState.NoRuleIDs).
//
// Failures reach a condition or an Event only as the fixed texts of package
// s3client (a category, or the HTTP status of a refusal); the log names the
// operation and the category or the API's reason at V(1), never a host, a key
// or the text of an answer.
package gcs

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"net/http"
	"reflect"
	"slices"
	"strings"

	"cloud.google.com/go/storage"
	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/craftainer-labs/lib-operator-go/pkg/cloud"

	"github.com/craftainer-labs/operator-s3/internal/s3client"
)

var (
	// ErrQuotaUnsupported means the request carries a bucket quota, which GCS
	// cannot enforce. Remove spec.quota from the Bucket.
	ErrQuotaUnsupported = errors.New("a GCP store has no bucket quota: remove spec.quota")
	// ErrObjectLockUnsupported means the request asks for object lock, which
	// has no GCS counterpart. Remove spec.objectLock from the Bucket.
	ErrObjectLockUnsupported = errors.New("a GCP store has no object lock: remove spec.objectLock")
	// ErrClientNotBuilt is the answer of New when the library refuses its
	// configuration (a malformed endpoint, for one).
	ErrClientNotBuilt = errors.New("cannot build the GCS client from its configuration")
	// ErrAccessNotImplemented is the answer of ApplyAccess: BucketAccess on GCP
	// is not implemented until its API is decided.
	ErrAccessNotImplemented = errors.New("access on a GCP store is not implemented yet: BucketAccess on GCP awaits an API decision")
)

// LabelMarker is the bucket label that records which claim created the bucket.
// GCS label keys cannot hold the "." and "/" of s3client.MarkerTag.
const LabelMarker = "craftainer-dev-claim"

// Store is the buckets of one Google Cloud project. It is safe for concurrent
// use.
type Store struct {
	client            *storage.Client
	hc                *http.Client
	project, location string
}

// New builds the Store of a resolved GCP client (see cloud.Egress.GCP). It
// makes no request. One attempt per call: the controller retries with backoff.
// location is where a bucket is created; empty is GCS's default, "US".
func New(ctx context.Context, c *cloud.GCPClient, project, location string) (*Store, error) {
	// The client already authorizes its requests: the library must not look
	// for credentials of its own (which could reach the metadata server).
	opts := []option.ClientOption{option.WithHTTPClient(c.HTTPClient), option.WithoutAuthentication()}
	if c.Endpoint != "" {
		opts = append(opts, option.WithEndpoint(strings.TrimRight(c.Endpoint, "/")+"/storage/v1/"))
	}
	cl, err := storage.NewClient(ctx, opts...)
	if err != nil {
		// The library's text can carry the configured endpoint: a fixed one.
		return nil, ErrClientNotBuilt
	}
	cl.SetRetry(storage.WithMaxAttempts(1))
	return &Store{client: cl, hc: c.HTTPClient, project: project, location: location}, nil
}

// MarkerAtCreate marks the Store as one that writes a bucket's label in the
// request that creates it, so an unlabelled bucket is never one of its claims.
func (*Store) MarkerAtCreate() {}

// CloseIdleConnections closes the idle connections of the Store's HTTP client;
// the controllers call it when they drop a cached Store.
func (s *Store) CloseIdleConnections() { s.hc.CloseIdleConnections() }

// EncodeMarker is the form of a claim marker that the bucket's label holds: a
// digest, because a label value cannot hold "/", an uppercase letter or more
// than 63 characters.
func (*Store) EncodeMarker(marker string) string {
	sum := sha256.Sum256([]byte(marker))
	return hex.EncodeToString(sum[:20])
}

// statusOf is the HTTP status of an error of the library that carries an
// answer, 0 for one that does not (a request that never got one).
func statusOf(err error) int {
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		return ge.Code
	}
	return 0
}

// reasonOf is the reason of the first error of an answer, "" when it has none.
func reasonOf(err error) string {
	var ge *googleapi.Error
	if errors.As(err, &ge) && len(ge.Errors) > 0 {
		return ge.Errors[0].Reason
	}
	return ""
}

// tokenRefusal is whether the token endpoint refused the key itself (a 4xx
// other than 429); its 5xx and 429 answers are transient, so the store stays
// unreachable and the call is retried.
func tokenRefusal(e *oauth2.RetrieveError) bool {
	if e.Response == nil {
		return false
	}
	c := e.Response.StatusCode
	return c >= 400 && c < 500 && c != http.StatusTooManyRequests
}

// heldElsewhere is whether a 403 on a bucket read can mean that another
// project holds the name: not a rate limit or a quota, which also answer 403.
func heldElsewhere(err error) bool {
	if statusOf(err) != http.StatusForbidden {
		return false
	}
	r := strings.ToLower(reasonOf(err))
	return !strings.Contains(r, "limit") && !strings.Contains(r, "quota")
}

// fail turns an error of the library into the errors of package s3client and
// logs its category. 401 and 403 are an authentication failure of an
// unreachable store, as for the other backends.
func fail(ctx context.Context, op string, err error) error {
	status := statusOf(err)
	var tokenRefused *oauth2.RetrieveError
	if status == 0 && errors.As(err, &tokenRefused) && tokenRefusal(tokenRefused) {
		// The token endpoint refused the service-account key (it has no
		// googleapi status): a failed authentication, not a network failure.
		log.FromContext(ctx).V(1).Info("the token endpoint refused the credentials", "request", op)
		return s3client.AuthFailed(&s3client.Error{Status: http.StatusUnauthorized})
	}
	if status == 0 {
		e := s3client.Unreachable(err)
		log.FromContext(ctx).V(1).Info("cannot reach the backend", "request", op, "error", e.Error())
		return e
	}
	e := &s3client.Error{Status: status, Code: reasonOf(err)}
	log.FromContext(ctx).V(1).Info("the backend refused the request", "request", op, "status", status, "code", s3client.LoggableCode(e.Code))
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return s3client.AuthFailed(e)
	}
	return e
}

// Bucket reads the bucket name: its existence, labels, versioning and lifecycle
// rules. The quota is always 0: GCS has none. A bucket the credentials cannot
// see (another project's) answers 403: when the credentials can still list the
// project's buckets the 403 means the name is held elsewhere, which is
// s3client.ErrBucketExists (a name collision, never managed and never blocking
// a delete); otherwise it is an authentication failure.
func (s *Store) Bucket(ctx context.Context, name string) (s3client.BucketState, error) {
	attrs, err := s.client.Bucket(name).Attrs(ctx)
	if errors.Is(err, storage.ErrBucketNotExist) || statusOf(err) == http.StatusNotFound {
		return s3client.BucketState{}, nil
	}
	if heldElsewhere(err) {
		// A key that can list the project's buckets but not read this one is
		// taken for a name held elsewhere: the safe side, as the bucket is then
		// never managed. Another list failure is the answer.
		if _, perr := s.ServerVersion(ctx); !errors.Is(perr, s3client.ErrNoVersion) {
			return s3client.BucketState{}, perr
		}
		return s3client.BucketState{}, s3client.ErrBucketExists
	}
	if err != nil {
		return s3client.BucketState{}, fail(ctx, "GET bucket", err)
	}
	st := s3client.BucketState{
		Exists: true, Tags: map[string]string{}, Versioning: attrs.VersioningEnabled, NoRuleIDs: true,
		Drifted: !attrs.UniformBucketLevelAccess.Enabled || attrs.PublicAccessPrevention != storage.PublicAccessPreventionEnforced,
	}
	for k, v := range attrs.Labels {
		if k == LabelMarker {
			k = s3client.MarkerTag
		}
		st.Tags[k] = v
	}
	for _, r := range attrs.Lifecycle.Rules {
		st.Rules = append(st.Rules, ruleOf(r))
	}
	slices.SortFunc(st.Rules, func(a, b s3client.Rule) int {
		return cmp.Or(strings.Compare(a.Prefix, b.Prefix), cmp.Compare(a.Days, b.Days))
	})
	return st, nil
}

// ruleOf is a lifecycle rule as the operator writes them: a delete after an
// age in days, under a prefix. A rule of any other shape (another action or
// condition, several prefixes) is reported as Disabled: the operator cannot
// express it, so it differs from every rule the claim asks for and is
// replaced, as the claim owns the bucket's lifecycle configuration.
func ruleOf(r storage.LifecycleRule) s3client.Rule {
	c := r.Condition
	out := s3client.Rule{Days: int32(min(max(c.AgeInDays, 0), math.MaxInt32))}
	if len(c.MatchesPrefix) == 1 {
		out.Prefix = c.MatchesPrefix[0]
	}
	rest := c
	rest.AgeInDays, rest.MatchesPrefix = 0, nil
	out.Disabled = r.Action.Type != storage.DeleteAction || c.AgeInDays <= 0 || len(c.MatchesPrefix) > 1 ||
		!reflect.DeepEqual(rest, storage.LifecycleCondition{})
	return out
}

func lifecycleOf(rules []s3client.Rule) storage.Lifecycle {
	var l storage.Lifecycle
	for _, r := range rules {
		c := storage.LifecycleCondition{AgeInDays: int64(r.Days)}
		if r.Prefix != "" {
			c.MatchesPrefix = []string{r.Prefix}
		}
		l.Rules = append(l.Rules, storage.LifecycleRule{Action: storage.LifecycleAction{Type: storage.DeleteAction}, Condition: c})
	}
	return l
}

// Validate refuses parameters the store can never apply, before any request is
// sent: a quota (ErrQuotaUnsupported) and object lock (ErrObjectLockUnsupported).
func (*Store) Validate(p s3client.BucketParams) error {
	switch {
	case p.Quota != 0:
		return ErrQuotaUnsupported
	case p.ObjectLock:
		return ErrObjectLockUnsupported
	}
	return nil
}

// CreateBucket makes the bucket in the store's location with its label,
// versioning and lifecycle rules in one request. A bucket that exists already
// is s3client.ErrBucketExists, whoever made it: GCS answers 409 both for the
// project's own bucket and for a name another project holds.
func (s *Store) CreateBucket(ctx context.Context, p s3client.BucketParams) error {
	if err := s.Validate(p); err != nil {
		return err
	}
	attrs := &storage.BucketAttrs{
		Location: s.location, VersioningEnabled: p.Versioning,
		Labels: map[string]string{LabelMarker: p.Marker}, Lifecycle: lifecycleOf(p.Rules),
		// Hardened as new S3 buckets are: no object ACLs, no public access.
		UniformBucketLevelAccess: storage.UniformBucketLevelAccess{Enabled: true},
		PublicAccessPrevention:   storage.PublicAccessPreventionEnforced,
	}
	if err := s.client.Bucket(p.Name).Create(ctx, s.project, attrs); err != nil {
		if statusOf(err) == http.StatusConflict {
			return s3client.ErrBucketExists
		}
		return fail(ctx, "POST bucket", err)
	}
	return nil
}

// UpdateBucket changes what differs between the bucket as Bucket found it and
// the parameters, in one request. Labels other than the marker are kept.
func (s *Store) UpdateBucket(ctx context.Context, p s3client.BucketParams, cur s3client.BucketState) error {
	if err := s.Validate(p); err != nil {
		return err
	}
	var u storage.BucketAttrsToUpdate
	changed := false
	if cur.Marker() != p.Marker {
		u.SetLabel(LabelMarker, p.Marker)
		changed = true
	}
	if cur.Versioning != p.Versioning {
		u.VersioningEnabled = p.Versioning
		changed = true
	}
	// Only a bucket this claim owns (its label) is hardened again.
	if cur.Drifted && cur.Marker() == p.Marker {
		u.UniformBucketLevelAccess = &storage.UniformBucketLevelAccess{Enabled: true}
		u.PublicAccessPrevention = storage.PublicAccessPreventionEnforced
		changed = true
	}
	// GCS rules have no ID, whatever the caller's copy of the state says.
	cur.NoRuleIDs = true
	if !s3client.RulesMatch(cur, p.Rules) {
		l := lifecycleOf(p.Rules)
		u.Lifecycle = &l
		changed = true
	}
	if !changed {
		return nil
	}
	if _, err := s.client.Bucket(p.Name).Update(ctx, u); err != nil {
		return fail(ctx, "PATCH bucket", err)
	}
	return nil
}

// DeleteBucket deletes an empty bucket; one that is gone is success. A bucket
// that holds objects is s3client.ErrBucketNotEmpty: the operator never deletes
// objects.
func (s *Store) DeleteBucket(ctx context.Context, name string) error {
	err := s.client.Bucket(name).Delete(ctx)
	switch {
	case err == nil, errors.Is(err, storage.ErrBucketNotExist), statusOf(err) == http.StatusNotFound:
		return nil
	case statusOf(err) == http.StatusConflict:
		return s3client.ErrBucketNotEmpty
	}
	return fail(ctx, "DELETE bucket", err)
}

// ServerVersion is an authenticated read of the project's buckets (one page of
// one). GCS has no version: a success is s3client.ErrNoVersion, which the probe
// reads as a healthy store with an empty version.
func (s *Store) ServerVersion(ctx context.Context) (string, error) {
	it := s.client.Buckets(ctx, s.project)
	it.PageInfo().MaxSize = 1
	if _, err := it.Next(); err != nil && !errors.Is(err, iterator.Done) {
		return "", fail(ctx, "GET buckets", err)
	}
	return "", s3client.ErrNoVersion
}

// User finds no user: a GCP store has no BucketAccess accounts yet, so a claim
// that waits for one can be deleted.
func (*Store) User(context.Context, string) (s3client.UserState, error) {
	return s3client.UserState{}, nil
}

// ApplyAccess is not implemented: ErrAccessNotImplemented.
func (*Store) ApplyAccess(context.Context, s3client.AccessParams, s3client.UserState) error {
	return ErrAccessNotImplemented
}

// OwnedPolicies finds no policy.
func (*Store) OwnedPolicies(context.Context, string) ([]string, error) { return nil, nil }

// DeleteAccess has nothing to delete.
func (*Store) DeleteAccess(context.Context, string, []string) error { return nil }
