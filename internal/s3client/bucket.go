package s3client

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
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

func bucketPath(name string) string { return "/" + name }

// sub is the query of a bucket sub-resource such as ?tagging.
func sub(name string) url.Values { return url.Values{name: {""}} }

type tag struct {
	Key   string `xml:"Key"`
	Value string `xml:"Value"`
}

type tagging struct {
	XMLName xml.Name `xml:"Tagging"`
	Tags    []tag    `xml:"TagSet>Tag"`
}

type versioning struct {
	XMLName xml.Name `xml:"VersioningConfiguration"`
	Status  string   `xml:"Status"`
}

type lifecycleRule struct {
	ID     string `xml:"ID"`
	Prefix string `xml:"Prefix,omitempty"`
	Filter struct {
		Prefix string `xml:"Prefix"`
	} `xml:"Filter"`
	Status     string `xml:"Status"`
	Expiration struct {
		Days int32 `xml:"Days"`
	} `xml:"Expiration"`
}

type lifecycle struct {
	XMLName xml.Name        `xml:"LifecycleConfiguration"`
	Rules   []lifecycleRule `xml:"Rule"`
}

func decodeXML(data []byte, v any) error {
	if err := xml.Unmarshal(data, v); err != nil {
		return ErrBadResponse
	}
	return nil
}

// getXML reads a bucket sub-resource into v. A 404 leaves v empty: the
// sub-resource has no configuration (the bucket's existence is checked
// first).
func (c *Client) getXML(ctx context.Context, bucket, resource string, v any) error {
	data, err := c.do(ctx, call{method: http.MethodGet, path: bucketPath(bucket), query: sub(resource)})
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return decodeXML(data, v)
}

// putXML writes a bucket sub-resource. The SHA-256 checksum stands in for the
// Content-MD5 older S3 implementations ask of tagging and lifecycle writes.
func (c *Client) putXML(ctx context.Context, bucket, resource string, v any) error {
	body, _ := xml.Marshal(v) // plain strings and integers always marshal
	sum := sha256.Sum256(body)
	_, err := c.do(ctx, call{
		method: http.MethodPut, path: bucketPath(bucket), query: sub(resource), body: body, contentType: "application/xml",
		header: map[string]string{
			"X-Amz-Sdk-Checksum-Algorithm": "SHA256",
			"X-Amz-Checksum-Sha256":        base64.StdEncoding.EncodeToString(sum[:]),
		},
	})
	return err
}

// Bucket reads the bucket name: its existence, tags, versioning, lifecycle
// rules and quota.
func (c *Client) Bucket(ctx context.Context, name string) (BucketState, error) {
	_, err := c.do(ctx, call{method: http.MethodHead, path: bucketPath(name)})
	if errors.Is(err, ErrNotFound) {
		return BucketState{}, nil
	}
	if err != nil {
		return BucketState{}, err
	}
	st := BucketState{Exists: true, Tags: map[string]string{}}
	var tags tagging
	var ver versioning
	var life lifecycle
	for _, read := range []func() error{
		func() error { return c.getXML(ctx, name, "tagging", &tags) },
		func() error { return c.getXML(ctx, name, "versioning", &ver) },
		func() error { return c.getXML(ctx, name, "lifecycle", &life) },
		func() (err error) {
			st.Quota, err = c.quota(ctx, name)
			return err
		},
	} {
		if err := read(); err != nil {
			return BucketState{}, err
		}
	}
	for _, t := range tags.Tags {
		st.Tags[t.Key] = t.Value
	}
	st.Versioning = ver.Status == "Enabled"
	for _, r := range life.Rules {
		prefix := r.Filter.Prefix
		if prefix == "" {
			prefix = r.Prefix
		}
		st.Rules = append(st.Rules, Rule{ID: r.ID, Prefix: prefix, Days: r.Expiration.Days, Disabled: r.Status != "Enabled"})
	}
	st.Rules = sortedRules(st.Rules)
	return st, nil
}

// CreateBucket makes the bucket and applies the rest of the parameters.
func (c *Client) CreateBucket(ctx context.Context, p BucketParams) error {
	rq := call{method: http.MethodPut, path: bucketPath(p.Name)}
	if p.ObjectLock {
		rq.header = map[string]string{"X-Amz-Bucket-Object-Lock-Enabled": "true"}
	}
	if _, err := c.do(ctx, rq); err != nil {
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
		status := "Suspended"
		if p.Versioning {
			status = "Enabled"
		}
		steps = append(steps, func() error { return c.putXML(ctx, p.Name, "versioning", versioning{Status: status}) })
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
	var t tagging
	for _, k := range slices.Sorted(maps.Keys(all)) {
		t.Tags = append(t.Tags, tag{Key: k, Value: all[k]})
	}
	return c.putXML(ctx, p.Name, "tagging", t)
}

func (c *Client) putRules(ctx context.Context, p BucketParams) error {
	if len(p.Rules) == 0 {
		_, err := c.do(ctx, call{method: http.MethodDelete, path: bucketPath(p.Name), query: sub("lifecycle")})
		return ignoreNotFound(err)
	}
	var l lifecycle
	for _, r := range sortedRules(p.Rules) {
		lr := lifecycleRule{ID: r.ID, Status: "Enabled"}
		lr.Filter.Prefix = r.Prefix
		lr.Expiration.Days = r.Days
		l.Rules = append(l.Rules, lr)
	}
	return c.putXML(ctx, p.Name, "lifecycle", l)
}

// DeleteBucket deletes an empty bucket; one that is gone is success. A bucket
// that holds objects is ErrBucketNotEmpty: the operator never deletes objects.
func (c *Client) DeleteBucket(ctx context.Context, name string) error {
	_, err := c.do(ctx, call{method: http.MethodDelete, path: bucketPath(name)})
	var apiErr *Error
	if errors.As(err, &apiErr) && apiErr.Code == "BucketNotEmpty" {
		return ErrBucketNotEmpty
	}
	return ignoreNotFound(err)
}
