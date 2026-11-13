package s3client

import (
	"context"
	"errors"
	"maps"
	"math"
	"slices"
	"strings"

	"github.com/minio/minio-go/v7"
	mlifecycle "github.com/minio/minio-go/v7/pkg/lifecycle"
	"github.com/minio/minio-go/v7/pkg/tags"
)

// MarkerTag is the bucket tag that records which claim created the bucket
// (ADR 0002 section 10): "<namespace>/<name>/<uid>".
const MarkerTag = "craftainer.dev/claim"

// Rule expires the objects under Prefix Days days after their creation.
type Rule struct {
	ID, Prefix string
	Days       int32
	// Disabled is a rule the store has but does not apply; the operator only
	// ever writes enabled rules.
	Disabled bool
}

// BucketState is what the store reports about a bucket.
type BucketState struct {
	Exists     bool
	Tags       map[string]string
	Versioning bool
	// Rules are sorted by ID.
	Rules []Rule
	// Quota is the hard quota in bytes; 0 is none.
	Quota int64
}

// Marker is the claim recorded in the MarkerTag, or "".
func (s BucketState) Marker() string { return s.Tags[MarkerTag] }

// BucketParams is what a Bucket claim wants.
type BucketParams struct {
	Name, Marker string
	Versioning   bool
	// ObjectLock is applied when the bucket is created and never changed.
	ObjectLock bool
	Quota      int64
	Rules      []Rule
}

func sortedRules(r []Rule) []Rule {
	r = slices.Clone(r)
	slices.SortFunc(r, func(a, b Rule) int { return strings.Compare(a.ID, b.ID) })
	return r
}

// RulesEqual reports whether two rule lists hold the same rules, in any order.
func RulesEqual(a, b []Rule) bool { return slices.Equal(sortedRules(a), sortedRules(b)) }

// Bucket reads the bucket name: its existence, tags, versioning, lifecycle
// rules and quota.
func (c *Client) Bucket(ctx context.Context, name string) (BucketState, error) {
	ok, err := c.mc.BucketExists(ctx, name)
	if err = c.s3(ctx, "HEAD bucket", err); err != nil || !ok {
		return BucketState{}, err
	}
	st := BucketState{Exists: true, Tags: map[string]string{}}
	for _, read := range []func() error{
		func() error { return c.readTags(ctx, name, &st) },
		func() error { return c.readVersioning(ctx, name, &st) },
		func() error { return c.readRules(ctx, name, &st) },
		func() (err error) {
			st.Quota, err = c.quota(ctx, name)
			return err
		},
	} {
		if err := read(); err != nil {
			return BucketState{}, err
		}
	}
	return st, nil
}

// A 404 on a sub-resource means the bucket has no configuration of that kind.

func (c *Client) readTags(ctx context.Context, name string, st *BucketState) error {
	t, err := c.mc.GetBucketTagging(ctx, name)
	if err = ignoreNotFound(c.s3(ctx, "GET tagging", err)); err != nil {
		return err
	}
	if t != nil {
		maps.Copy(st.Tags, t.ToMap())
	}
	return nil
}

func (c *Client) readVersioning(ctx context.Context, name string, st *BucketState) error {
	v, err := c.mc.GetBucketVersioning(ctx, name)
	st.Versioning = v.Enabled()
	return c.s3(ctx, "GET versioning", err)
}

func (c *Client) readRules(ctx context.Context, name string, st *BucketState) error {
	cfg, err := c.mc.GetBucketLifecycle(ctx, name)
	if err = ignoreNotFound(c.s3(ctx, "GET lifecycle", err)); err != nil || cfg == nil {
		return err
	}
	for _, r := range cfg.Rules {
		prefix := r.RuleFilter.Prefix
		if prefix == "" {
			prefix = r.Prefix
		}
		st.Rules = append(st.Rules, Rule{ID: r.ID, Prefix: prefix, Days: clampDays(int(r.Expiration.Days)), Disabled: r.Status != "Enabled"})
	}
	st.Rules = sortedRules(st.Rules)
	return nil
}

// CreateBucket makes the bucket and applies the rest of the parameters.
func (c *Client) CreateBucket(ctx context.Context, p BucketParams) error {
	err := c.mc.MakeBucket(ctx, p.Name, minio.MakeBucketOptions{Region: c.region, ObjectLocking: p.ObjectLock})
	if err := c.s3(ctx, "PUT bucket", err); err != nil {
		return err
	}
	return c.configure(ctx, p, BucketState{})
}

// UpdateBucket changes what differs between the bucket as Bucket found it and
// the parameters. Tags other than the marker are kept.
func (c *Client) UpdateBucket(ctx context.Context, p BucketParams, cur BucketState) error {
	return c.configure(ctx, p, cur)
}

func (c *Client) configure(ctx context.Context, p BucketParams, cur BucketState) error {
	var steps []func() error
	if cur.Marker() != p.Marker {
		steps = append(steps, func() error { return c.putTags(ctx, p, cur.Tags) })
	}
	if cur.Versioning != p.Versioning {
		steps = append(steps, func() error { return c.putVersioning(ctx, p) })
	}
	if !RulesEqual(cur.Rules, p.Rules) {
		steps = append(steps, func() error { return c.putRules(ctx, p) })
	}
	if cur.Quota != p.Quota {
		steps = append(steps, func() error { return c.setQuota(ctx, p.Name, p.Quota) })
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) putTags(ctx context.Context, p BucketParams, existing map[string]string) error {
	all := maps.Clone(existing)
	if all == nil {
		all = map[string]string{}
	}
	all[MarkerTag] = p.Marker
	t, err := tags.NewTags(all, false)
	if err != nil {
		return ErrInvalidTag
	}
	return c.s3(ctx, "PUT tagging", c.mc.SetBucketTagging(ctx, p.Name, t))
}

func (c *Client) putVersioning(ctx context.Context, p BucketParams) error {
	if p.Versioning {
		return c.s3(ctx, "PUT versioning", c.mc.EnableVersioning(ctx, p.Name))
	}
	return c.s3(ctx, "PUT versioning", c.mc.SuspendVersioning(ctx, p.Name))
}

// putRules sets the lifecycle rules; no rules removes the configuration.
func (c *Client) putRules(ctx context.Context, p BucketParams) error {
	cfg := mlifecycle.NewConfiguration()
	for _, r := range sortedRules(p.Rules) {
		cfg.Rules = append(cfg.Rules, mlifecycle.Rule{
			ID: r.ID, Status: "Enabled", RuleFilter: mlifecycle.Filter{Prefix: r.Prefix},
			Expiration: mlifecycle.Expiration{Days: mlifecycle.ExpirationDays(r.Days)},
		})
	}
	return ignoreNotFound(c.s3(ctx, "PUT lifecycle", c.mc.SetBucketLifecycle(ctx, p.Name, cfg)))
}

// DeleteBucket deletes an empty bucket; one that is gone is success. A bucket
// that holds objects is ErrBucketNotEmpty: the operator never deletes objects.
func (c *Client) DeleteBucket(ctx context.Context, name string) error {
	err := c.s3(ctx, "DELETE bucket", c.mc.RemoveBucket(ctx, name))
	var apiErr *Error
	if errors.As(err, &apiErr) && apiErr.Code == "BucketNotEmpty" {
		return ErrBucketNotEmpty
	}
	return ignoreNotFound(err)
}

// clampDays narrows a lifecycle day count to int32, saturating instead of
// wrapping.
func clampDays(d int) int32 {
	if d > math.MaxInt32 {
		return math.MaxInt32
	}
	if d < 0 {
		return 0
	}
	return int32(d)
}
