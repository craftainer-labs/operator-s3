// Package awss3 is the object store backend for an AWS account (NKs3-1a): the
// buckets of a store whose provider is AWS, through aws-sdk-go-v2's S3 client.
// The AWS configuration (region, credentials, role, egress policy, proxy and
// the emulator endpoint of the tests) comes from pkg/cloud's Egress.AWS, so
// every request dials through the egress policy.
//
// A Store has the method set of controller.Store. The IAM methods (users,
// policies and access keys) are not implemented until NKs3-1b and answer
// ErrNotImplemented. A bucket quota has no AWS equivalent, so a Store refuses a
// request that carries one (ErrQuotaUnsupported) rather than ignore it: the
// bucket would never report the quota back and the claim would be updated
// forever.
//
// Failures reach a condition or an Event only as the fixed texts of package
// s3client (a category, or the HTTP status of a refusal); the log names the
// operation and the category or the S3 error code at V(1), never a host, a key
// or the text of an answer.
package awss3

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/craftainer-labs/operator-s3/internal/s3client"
)

var (
	// ErrNotImplemented is the answer of every IAM method: access keys on an
	// AWS store arrive with NKs3-1b.
	ErrNotImplemented = errors.New("access keys on an AWS store are not implemented yet")
	// ErrQuotaUnsupported means the request carries a bucket quota, which S3
	// cannot enforce. Remove spec.quota from the Bucket.
	ErrQuotaUnsupported = errors.New("an AWS store has no bucket quota: remove spec.quota")
)

// defaultRegion is the one region whose CreateBucket takes no location
// constraint (a constraint of us-east-1 is refused as invalid).
const defaultRegion = "us-east-1"

// Store is the buckets of one AWS account and region. It is safe for
// concurrent use.
type Store struct {
	api    *s3.Client
	region string
	hc     aws.HTTPClient
}

// New builds the Store of an AWS configuration (see cloud.Egress.AWS). It makes
// no request. One attempt per call: the controller retries with backoff.
// Checksums are calculated only where S3 requires them, which an S3 compatible
// emulator can parse. A configuration with a base endpoint (the emulator
// override) addresses buckets path-style, because an emulator has no wildcard
// DNS; AWS itself is addressed virtual-hosted.
func New(cfg aws.Config) *Store {
	api := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.RetryMaxAttempts = 1
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
		o.UsePathStyle = cfg.BaseEndpoint != nil
	})
	return &Store{api: api, region: cfg.Region, hc: cfg.HTTPClient}
}

// CloseIdleConnections closes the idle connections of the Store's HTTP client;
// the controllers call it when they drop a cached Store.
func (s *Store) CloseIdleConnections() { closeIdle(s.hc) }

func closeIdle(hc aws.HTTPClient) {
	if c, ok := hc.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

// statusOf is the HTTP status of an error of the SDK that carries an answer, 0
// for one that does not (a request that never got one).
func statusOf(err error) int {
	var resp interface{ HTTPStatusCode() int }
	if errors.As(err, &resp) {
		return resp.HTTPStatusCode()
	}
	return 0
}

// codeOf is the S3 error code of an error of the SDK, "" when it has none.
func codeOf(err error) string {
	var api smithy.APIError
	if errors.As(err, &api) {
		return api.ErrorCode()
	}
	return ""
}

// fail turns an error of the SDK into the errors of package s3client and logs
// its category. 401 and 403 are an authentication failure of an unreachable
// store, as for a RustFS store.
func fail(ctx context.Context, op string, err error) error {
	status := statusOf(err)
	if status == 0 {
		e := s3client.Unreachable(err)
		log.FromContext(ctx).V(1).Info("cannot reach the backend", "request", op, "error", e.Error())
		return e
	}
	e := &s3client.Error{Status: status, Code: codeOf(err)}
	log.FromContext(ctx).V(1).Info("the backend refused the request", "request", op, "status", status, "code", s3client.LoggableCode(e.Code))
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return s3client.AuthFailed(e)
	}
	return e
}

// head reports whether the bucket exists. A bucket that answers 403 (somebody
// else's) or 301 (in another region) is not this account's to touch:
// ErrBucketExists. A 403 is also what refused credentials answer, which the
// probe of the store reports (Ready=False) separately.
func (s *Store) head(ctx context.Context, name string) (bool, error) {
	_, err := s.api.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(name)})
	if err == nil {
		return true, nil
	}
	switch statusOf(err) {
	case http.StatusNotFound:
		return false, nil
	case http.StatusForbidden, http.StatusMovedPermanently:
		return false, s3client.ErrBucketExists
	}
	return false, fail(ctx, "HEAD bucket", err)
}

// Bucket reads the bucket name: its existence, tags, versioning and lifecycle
// rules. The quota is always 0: S3 has none. A bucket of another account is
// s3client.ErrBucketExists.
func (s *Store) Bucket(ctx context.Context, name string) (s3client.BucketState, error) {
	ok, err := s.head(ctx, name)
	if err != nil || !ok {
		return s3client.BucketState{}, err
	}
	st := s3client.BucketState{Exists: true, Tags: map[string]string{}}
	for _, read := range []func() error{
		func() error { return s.readTags(ctx, name, &st) },
		func() error { return s.readVersioning(ctx, name, &st) },
		func() error { return s.readRules(ctx, name, &st) },
	} {
		if err := read(); err != nil {
			return s3client.BucketState{}, err
		}
	}
	return st, nil
}

// A bucket without tags or lifecycle rules answers with an error code of its
// own, which is not a failure.

func (s *Store) readTags(ctx context.Context, name string, st *s3client.BucketState) error {
	out, err := s.api.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: aws.String(name)})
	if codeOf(err) == "NoSuchTagSet" {
		return nil
	}
	if err != nil {
		return fail(ctx, "GET tagging", err)
	}
	for _, t := range out.TagSet {
		st.Tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return nil
}

func (s *Store) readVersioning(ctx context.Context, name string, st *s3client.BucketState) error {
	out, err := s.api.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: aws.String(name)})
	if err != nil {
		return fail(ctx, "GET versioning", err)
	}
	st.Versioning = out.Status == types.BucketVersioningStatusEnabled
	return nil
}

func (s *Store) readRules(ctx context.Context, name string, st *s3client.BucketState) error {
	out, err := s.api.GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{Bucket: aws.String(name)})
	if codeOf(err) == "NoSuchLifecycleConfiguration" {
		return nil
	}
	if err != nil {
		return fail(ctx, "GET lifecycle", err)
	}
	for _, r := range out.Rules {
		st.Rules = append(st.Rules, ruleOf(r))
	}
	slices.SortFunc(st.Rules, func(a, b s3client.Rule) int { return strings.Compare(a.ID, b.ID) })
	return nil
}

// ruleOf is a lifecycle rule as the operator writes them: an expiration in days
// under a prefix filter. A rule of any other shape (a tag, size or compound
// filter, no filter, no expiration in days) is reported as Disabled: the
// operator cannot express it, so it differs from every rule the claim asks for
// and is replaced, as the claim owns the bucket's lifecycle configuration.
func ruleOf(r types.LifecycleRule) s3client.Rule {
	out := s3client.Rule{ID: aws.ToString(r.ID), Disabled: r.Status != types.ExpirationStatusEnabled}
	f := r.Filter
	if f == nil || f.And != nil || f.Tag != nil || f.ObjectSizeGreaterThan != nil || f.ObjectSizeLessThan != nil ||
		r.Expiration == nil || r.Expiration.Days == nil {
		out.Disabled = true
		return out
	}
	out.Prefix = aws.ToString(f.Prefix)
	out.Days = aws.ToInt32(r.Expiration.Days)
	return out
}

// CreateBucket makes the bucket in the account's region and applies the rest
// of the parameters. A bucket that exists already is s3client.ErrBucketExists,
// whoever made it: in us-east-1 S3 answers a create of one's own bucket with
// success. The check is not atomic, so a creator holding the same credentials
// that wins the narrow gap between the check and the create is adopted; any
// other account gets BucketAlreadyExists, which is the same error.
func (s *Store) CreateBucket(ctx context.Context, p s3client.BucketParams) error {
	if p.Quota != 0 {
		return ErrQuotaUnsupported
	}
	exists, err := s.head(ctx, p.Name)
	if err != nil {
		return err
	}
	if exists {
		return s3client.ErrBucketExists
	}
	in := &s3.CreateBucketInput{Bucket: aws.String(p.Name)}
	if p.ObjectLock {
		in.ObjectLockEnabledForBucket = aws.Bool(true)
	}
	if s.region != defaultRegion {
		in.CreateBucketConfiguration = &types.CreateBucketConfiguration{LocationConstraint: types.BucketLocationConstraint(s.region)}
	}
	if _, err := s.api.CreateBucket(ctx, in); err != nil {
		if c := codeOf(err); c == "BucketAlreadyExists" || c == "BucketAlreadyOwnedByYou" {
			return s3client.ErrBucketExists
		}
		return fail(ctx, "PUT bucket", err)
	}
	return s.configure(ctx, p, s3client.BucketState{})
}

// UpdateBucket changes what differs between the bucket as Bucket found it and
// the parameters. Tags other than the marker are kept.
func (s *Store) UpdateBucket(ctx context.Context, p s3client.BucketParams, cur s3client.BucketState) error {
	if p.Quota != 0 {
		return ErrQuotaUnsupported
	}
	return s.configure(ctx, p, cur)
}

func (s *Store) configure(ctx context.Context, p s3client.BucketParams, cur s3client.BucketState) error {
	var steps []func() error
	if cur.Marker() != p.Marker {
		steps = append(steps, func() error { return s.putTags(ctx, p, cur.Tags) })
	}
	if cur.Versioning != p.Versioning {
		steps = append(steps, func() error { return s.putVersioning(ctx, p) })
	}
	if !s3client.RulesEqual(cur.Rules, p.Rules) {
		steps = append(steps, func() error { return s.putRules(ctx, p) })
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) putTags(ctx context.Context, p s3client.BucketParams, existing map[string]string) error {
	all := maps.Clone(existing)
	if all == nil {
		all = map[string]string{}
	}
	all[s3client.MarkerTag] = p.Marker
	set := make([]types.Tag, 0, len(all))
	for _, k := range slices.Sorted(maps.Keys(all)) {
		set = append(set, types.Tag{Key: aws.String(k), Value: aws.String(all[k])})
	}
	_, err := s.api.PutBucketTagging(ctx, &s3.PutBucketTaggingInput{Bucket: aws.String(p.Name), Tagging: &types.Tagging{TagSet: set}})
	if err != nil {
		return fail(ctx, "PUT tagging", err)
	}
	return nil
}

func (s *Store) putVersioning(ctx context.Context, p s3client.BucketParams) error {
	status := types.BucketVersioningStatusSuspended
	if p.Versioning {
		status = types.BucketVersioningStatusEnabled
	}
	_, err := s.api.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket: aws.String(p.Name), VersioningConfiguration: &types.VersioningConfiguration{Status: status},
	})
	if err != nil {
		return fail(ctx, "PUT versioning", err)
	}
	return nil
}

// putRules sets the lifecycle rules; no rules removes the configuration.
func (s *Store) putRules(ctx context.Context, p s3client.BucketParams) error {
	if len(p.Rules) == 0 {
		_, err := s.api.DeleteBucketLifecycle(ctx, &s3.DeleteBucketLifecycleInput{Bucket: aws.String(p.Name)})
		if err != nil {
			return fail(ctx, "DELETE lifecycle", err)
		}
		return nil
	}
	rules := make([]types.LifecycleRule, 0, len(p.Rules))
	for _, r := range p.Rules {
		rules = append(rules, types.LifecycleRule{
			ID: aws.String(r.ID), Status: types.ExpirationStatusEnabled,
			Filter:     &types.LifecycleRuleFilter{Prefix: aws.String(r.Prefix)},
			Expiration: &types.LifecycleExpiration{Days: aws.Int32(r.Days)},
		})
	}
	_, err := s.api.PutBucketLifecycleConfiguration(ctx, &s3.PutBucketLifecycleConfigurationInput{
		Bucket: aws.String(p.Name), LifecycleConfiguration: &types.BucketLifecycleConfiguration{Rules: rules},
	})
	if err != nil {
		return fail(ctx, "PUT lifecycle", err)
	}
	return nil
}

// DeleteBucket deletes an empty bucket; one that is gone is success. A bucket
// that holds objects, versions or delete markers is s3client.ErrBucketNotEmpty:
// the operator never deletes objects.
func (s *Store) DeleteBucket(ctx context.Context, name string) error {
	_, err := s.api.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(name)})
	switch {
	case err == nil, statusOf(err) == http.StatusNotFound:
		return nil
	case codeOf(err) == "BucketNotEmpty":
		return s3client.ErrBucketNotEmpty
	}
	return fail(ctx, "DELETE bucket", err)
}

// ServerVersion is an authenticated read of the account's buckets. S3 has no
// version: a success is s3client.ErrNoVersion, which the probe reads as a
// healthy store with an empty version.
func (s *Store) ServerVersion(ctx context.Context) (string, error) {
	if _, err := s.api.ListBuckets(ctx, &s3.ListBucketsInput{MaxBuckets: aws.Int32(1)}); err != nil {
		return "", fail(ctx, "GET buckets", err)
	}
	return "", s3client.ErrNoVersion
}

// The IAM methods wait for NKs3-1b.

// User is not implemented.
func (*Store) User(context.Context, string) (s3client.UserState, error) {
	return s3client.UserState{}, ErrNotImplemented
}

// ApplyAccess is not implemented.
func (*Store) ApplyAccess(context.Context, s3client.AccessParams, s3client.UserState) error {
	return ErrNotImplemented
}

// KeyExists is not implemented.
func (*Store) KeyExists(context.Context, string) (bool, error) { return false, ErrNotImplemented }

// IssueKey is not implemented.
func (*Store) IssueKey(context.Context, string, string, string) error { return ErrNotImplemented }

// DeleteKey is not implemented.
func (*Store) DeleteKey(context.Context, string) error { return ErrNotImplemented }

// OwnedPolicies is not implemented.
func (*Store) OwnedPolicies(context.Context, string) ([]string, error) { return nil, ErrNotImplemented }

// DeleteAccess is not implemented.
func (*Store) DeleteAccess(context.Context, string, []string) error { return ErrNotImplemented }
