package s3client

import (
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/craftainer-labs/lib-operator-go/pkg/lifecycle"
)

func TestBucketThatDoesNotExist(t *testing.T) {
	f := newFakeStore(t)
	st, err := f.client().Bucket(bg, "absent")
	if err != nil || st.Exists {
		t.Errorf("state = %+v, %v", st, err)
	}
}

func TestBucketLifecycle(t *testing.T) {
	f := newFakeStore(t)
	c := f.client()
	p := BucketParams{
		Name: "uploads", Marker: "ns/uploads/uid-1", Versioning: true, ObjectLock: true, Quota: 50 << 30,
		Rules: []Rule{{ID: "b-old", Prefix: "tmp/", Days: 7}, {ID: "a-logs", Prefix: "", Days: 30}},
	}

	// Create: the bucket with object lock, then the marker, versioning, rules
	// and quota.
	if err := c.CreateBucket(bg, p); err != nil {
		t.Fatal(err)
	}
	if !f.bucket["uploads"].lock {
		t.Error("the bucket was created without object lock")
	}
	st, err := c.Bucket(bg, "uploads")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Exists || st.Marker() != p.Marker || !st.Versioning || st.Quota != 50<<30 || !RulesEqual(st.Rules, p.Rules) {
		t.Errorf("state = %+v", st)
	}
	if st.Rules[0].ID != "a-logs" || st.Rules[1].Prefix != "tmp/" || st.Rules[1].Days != 7 || st.Rules[0].Disabled {
		t.Errorf("rules = %+v", st.Rules)
	}
	if f.quotaType != "HARD" {
		t.Errorf("quota type = %q", f.quotaType)
	}

	// Steady state: nothing is written.
	f.reset()
	if err := c.UpdateBucket(bg, p, st); err != nil {
		t.Fatal(err)
	}
	if len(f.requests()) != 0 {
		t.Errorf("a converged bucket was written: %v", f.requests())
	}

	// Drift: another tag is kept, the marker is set again, versioning is
	// suspended, the rules are removed and the quota cleared.
	f.bucket["uploads"].tags = map[string]string{"team": "a", MarkerTag: "other"}
	st, _ = c.Bucket(bg, "uploads")
	q := p
	q.Versioning, q.Rules, q.Quota = false, nil, 0
	f.reset()
	if err := c.UpdateBucket(bg, q, st); err != nil {
		t.Fatal(err)
	}
	b := f.bucket["uploads"]
	if b.tags["team"] != "a" || b.tags[MarkerTag] != p.Marker || b.versioning != "Suspended" || b.lifecycle != "" || len(f.quota) != 0 {
		t.Errorf("bucket = %+v, quota = %v", b, f.quota)
	}

	// Deleting a bucket with objects fails with a fixed error; an empty one is
	// deleted; one that is gone is success.
	b.objects = true
	err = c.DeleteBucket(bg, "uploads")
	if !errors.Is(err, ErrBucketNotEmpty) || strings.Contains(err.Error(), "BucketNotEmpty") {
		t.Errorf("not empty: %v", err)
	}
	b.objects = false
	if err := c.DeleteBucket(bg, "uploads"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteBucket(bg, "uploads"); err != nil {
		t.Errorf("repeat: %v", err)
	}
	f.failOn = "DELETE /uploads"
	if err := c.DeleteBucket(bg, "uploads"); err == nil {
		t.Error("a failing delete was not an error")
	}
}

// A bucket without a tag set, versioning status or lifecycle configuration,
// and a rule the store keeps disabled or writes in the legacy form.
func TestBucketReadsWhatStoresAnswer(t *testing.T) {
	f := newFakeStore(t)
	c := f.client()
	f.bucket["plain"] = &fakeBucket{tags: map[string]string{}}
	st, err := c.Bucket(bg, "plain")
	if err != nil || !st.Exists || st.Marker() != "" || st.Versioning || len(st.Rules) != 0 || st.Quota != 0 {
		t.Errorf("state = %+v, %v", st, err)
	}

	f.bucket["plain"].lifecycle = `<LifecycleConfiguration>
<Rule><ID>legacy</ID><Prefix>old/</Prefix><Status>Enabled</Status><Expiration><Days>3</Days></Expiration></Rule>
<Rule><ID>off</ID><Filter><Prefix>x/</Prefix></Filter><Status>Disabled</Status><Expiration><Days>9</Days></Expiration></Rule>
</LifecycleConfiguration>`
	st, _ = c.Bucket(bg, "plain")
	want := []Rule{{ID: "legacy", Prefix: "old/", Days: 3}, {ID: "off", Prefix: "x/", Days: 9, Disabled: true}}
	if !RulesEqual(st.Rules, want) {
		t.Errorf("rules = %+v", st.Rules)
	}
	// A disabled rule is drift.
	if RulesEqual(st.Rules, []Rule{{ID: "legacy", Prefix: "old/", Days: 3}, {ID: "off", Prefix: "x/", Days: 9}}) {
		t.Error("a disabled rule equals an enabled one")
	}
}

func TestBucketErrors(t *testing.T) {
	for _, failOn := range []string{"HEAD /bkt", "?tagging", "?versioning", "?lifecycle", "get-bucket-quota"} {
		f := newFakeStore(t)
		f.bucket["bkt"] = &fakeBucket{tags: map[string]string{"k": "v"}}
		f.failOn = failOn
		if _, err := f.client().Bucket(bg, "bkt"); err == nil {
			t.Errorf("%s: no error", failOn)
		}
	}
	// A body that does not parse is an unreachable store, not a crash.
	for _, resource := range []string{"tagging", "versioning", "lifecycle"} {
		f := newFakeStore(t)
		f.bucket["bkt"] = &fakeBucket{tags: map[string]string{"k": "v"}}
		f.setBody("GET /bkt/?"+resource, "<not-closed")
		if _, err := f.client().Bucket(bg, "bkt"); !errors.Is(err, lifecycle.ErrUnreachable) {
			t.Errorf("%s: %v", resource, err)
		}
	}
}

func TestCreateAndUpdateErrors(t *testing.T) {
	p := BucketParams{Name: "bkt", Marker: "m", Versioning: true, Quota: 5, Rules: []Rule{{ID: "r", Days: 1}}}
	for _, failOn := range []string{"PUT /bkt/", "tagging", "versioning", "lifecycle", "set-bucket-quota"} {
		f := newFakeStore(t)
		f.failOn = failOn
		if err := f.client().CreateBucket(bg, p); err == nil {
			t.Errorf("create, %s: no error", failOn)
		}
	}
	// Removing the lifecycle rules of a bucket that has none is fine, and a
	// failure to remove them is not.
	f := newFakeStore(t)
	f.bucket["bkt"] = &fakeBucket{tags: map[string]string{MarkerTag: "m"}}
	c := f.client()
	q := BucketParams{Name: "bkt", Marker: "m"}
	cur := BucketState{Exists: true, Tags: map[string]string{MarkerTag: "m"}, Rules: []Rule{{ID: "r", Days: 1}}}
	f.statusOn["DELETE /bkt/?lifecycle"] = 404
	if err := c.UpdateBucket(bg, q, cur); err != nil {
		t.Errorf("rules already gone: %v", err)
	}
	delete(f.statusOn, "DELETE /bkt/?lifecycle")
	f.failOn = "DELETE /bkt/?lifecycle"
	if err := c.UpdateBucket(bg, q, cur); err == nil {
		t.Error("a failing lifecycle delete was not an error")
	}
}

// Tags the store would refuse are refused before the request.
func TestInvalidTagsAreRefused(t *testing.T) {
	f := newFakeStore(t)
	err := f.client().putTags(bg, BucketParams{Name: "bkt", Marker: strings.Repeat("x", 300)}, nil)
	if !errors.Is(err, ErrInvalidTag) || len(f.requests()) != 0 {
		t.Errorf("err = %v, requests = %v", err, f.requests())
	}
}

func TestClampDays(t *testing.T) {
	big := math.MaxInt32
	big++
	for in, want := range map[int]int32{-1: 0, 0: 0, 30: 30, math.MaxInt32: math.MaxInt32, big: math.MaxInt32} {
		if got := clampDays(in); got != want {
			t.Errorf("clampDays(%d) = %d, want %d", in, got, want)
		}
	}
}
