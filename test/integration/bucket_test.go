//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/tags"

	"github.com/craftainer-labs/operator-s3/internal/s3client"
)

// create is what the Bucket controller does for a new claim: CreateBucket,
// then, while the server is not ready for a step, the reconciles that follow.
// RustFS answers a quota write with 503 until it has confirmed the capability
// across the cluster, which a fresh single node does a little after it starts.
func create(t *testing.T, c *s3client.Client, p s3client.BucketParams) {
	t.Helper()
	if err := c.CreateBucket(bg, p); err != nil {
		t.Logf("CreateBucket: %v (the controller would retry)", err)
	}
	eventually(t, 90*time.Second, "bucket "+p.Name+" converges", func() error {
		st, err := c.Bucket(bg, p.Name)
		if err != nil {
			return err
		}
		return c.UpdateBucket(bg, p, st)
	})
}

// A bucket made outside the operator reads as what it is: no tags, no
// versioning, no lifecycle rules and no quota, each of which the server
// answers in its own way (404 for no tags and no rules, 200 with an empty
// status or a zero quota for the others).
func TestBucketOfAnotherMaker(t *testing.T) {
	sv := load(t)
	c := sv.client(t)
	name := unique("it-plain")
	sv.makeBucket(t, name, false)

	st, err := c.Bucket(bg, name)
	if err != nil {
		t.Fatalf("Bucket: %v", err)
	}
	if !st.Exists || st.Marker() != "" || len(st.Tags) != 0 || st.Versioning || len(st.Rules) != 0 || st.Quota != 0 {
		t.Errorf("state = %+v", st)
	}
	if st, err := c.Bucket(bg, unique("it-absent")); err != nil || st.Exists {
		t.Errorf("an absent bucket = %+v, %v", st, err)
	}
}

// CreateBucket and UpdateBucket against the server: the marker tag, versioning,
// lifecycle rules (the XML both ways, with and without a prefix) and the quota
// are written and read back, tags that are not the marker are kept, and a
// converged bucket is left alone.
func TestBucketConfiguration(t *testing.T) {
	sv := load(t)
	c := sv.client(t)
	name := unique("it-cfg")
	marker := "it-ns/cfg/uid-1"
	sv.dropAfter(t, name)
	p := s3client.BucketParams{
		Name: name, Marker: marker, Versioning: true, Quota: 1 << 20,
		Rules: []s3client.Rule{{ID: "a-tmp", Prefix: "tmp/", Days: 7}, {ID: "b-all", Days: 30}},
	}
	create(t, c, p)

	st, err := c.Bucket(bg, name)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Exists || st.Marker() != marker || !st.Versioning || st.Quota != p.Quota || !s3client.RulesEqual(st.Rules, p.Rules) {
		t.Fatalf("state = %+v, want %+v", st, p)
	}
	for _, r := range st.Rules {
		if r.Disabled {
			t.Errorf("rule %+v is disabled", r)
		}
	}

	// A tag of somebody else's stays, and the marker is put right.
	mc := sv.admin(t)
	other, err := tags.NewTags(map[string]string{"team": "a", s3client.MarkerTag: "it-ns/other/uid-2"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := mc.SetBucketTagging(bg, name, other); err != nil {
		t.Fatalf("SetBucketTagging: %v", err)
	}
	st, err = c.Bucket(bg, name)
	if err != nil || st.Marker() != "it-ns/other/uid-2" || st.Tags["team"] != "a" {
		t.Fatalf("tags read back = %v, %v", st.Tags, err)
	}
	if err := c.UpdateBucket(bg, p, st); err != nil {
		t.Fatalf("UpdateBucket: %v", err)
	}
	st, err = c.Bucket(bg, name)
	if err != nil || st.Marker() != marker || st.Tags["team"] != "a" {
		t.Errorf("tags after = %v, %v", st.Tags, err)
	}

	// Steady state.
	if err := c.UpdateBucket(bg, p, st); err != nil {
		t.Errorf("a converged bucket: %v", err)
	}

	// Drift the other way: versioning suspended, the rules removed (no rules is
	// the end of the configuration) and the quota cleared.
	q := p
	q.Versioning, q.Rules, q.Quota = false, nil, 0
	if err := c.UpdateBucket(bg, q, st); err != nil {
		t.Fatalf("UpdateBucket to nothing: %v", err)
	}
	st, err = c.Bucket(bg, name)
	if err != nil || st.Versioning || len(st.Rules) != 0 || st.Quota != 0 || st.Marker() != marker {
		t.Errorf("state after = %+v, %v", st, err)
	}
	// Removing what is not there is not an error.
	if err := c.UpdateBucket(bg, q, st); err != nil {
		t.Errorf("repeat: %v", err)
	}
}

// Object lock is set when the bucket is created and implies versioning; a
// locked bucket cannot have its versioning suspended, which is why the
// controller always asks for it with the lock.
func TestObjectLock(t *testing.T) {
	sv := load(t)
	c := sv.client(t)
	name := unique("it-lock")
	sv.dropAfter(t, name)
	p := s3client.BucketParams{Name: name, Marker: "it-ns/lock/uid-1", Versioning: true, ObjectLock: true}
	create(t, c, p)

	st, err := c.Bucket(bg, name)
	if err != nil || !st.Exists || !st.Versioning {
		t.Fatalf("state = %+v, %v", st, err)
	}
	enabled, _, _, _, err := sv.admin(t).GetObjectLockConfig(bg, name)
	if err != nil || enabled != "Enabled" {
		t.Errorf("object lock = %q, %v", enabled, err)
	}
	q := p
	q.Versioning = false
	var apiErr *s3client.Error
	if err := c.UpdateBucket(bg, q, st); !errors.As(err, &apiErr) || apiErr.Status != http.StatusConflict {
		t.Errorf("suspending versioning under a lock: %v, want HTTP 409", err)
	}
}

// RustFS answers a create of a bucket that exists with success for the admin,
// so CreateBucket asks first and refuses; the bucket is left as it was.
func TestCreateOfAnExistingBucket(t *testing.T) {
	sv := load(t)
	c := sv.client(t)
	name := unique("it-exists")
	mc := sv.makeBucket(t, name, false)

	if err := mc.MakeBucket(bg, name, minio.MakeBucketOptions{Region: region}); err != nil {
		t.Logf("a create over an existing bucket by the admin: %v", err)
	} else {
		t.Log("a create over an existing bucket by the admin succeeds")
	}
	err := c.CreateBucket(bg, s3client.BucketParams{Name: name, Marker: "it-ns/exists/uid-1", Versioning: true})
	if !errors.Is(err, s3client.ErrBucketExists) {
		t.Fatalf("CreateBucket over an existing bucket: %v", err)
	}
	if st, err := c.Bucket(bg, name); err != nil || st.Marker() != "" || st.Versioning {
		t.Errorf("the bucket was changed: %+v, %v", st, err)
	}
}

// DeleteBucket refuses a bucket that holds objects, versions included, and
// leaves them; an empty one is deleted, and deleting what is gone is success.
func TestDeleteBucket(t *testing.T) {
	sv := load(t)
	c := sv.client(t)
	name := unique("it-delete")
	sv.dropAfter(t, name)
	create(t, c, s3client.BucketParams{Name: name, Marker: "it-ns/delete/uid-1", Versioning: true})
	mc := sv.admin(t)
	info, err := mc.PutObject(bg, name, "keep/me.txt", bytes.NewReader([]byte("data")), 4, minio.PutObjectOptions{})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	if err := c.DeleteBucket(bg, name); !errors.Is(err, s3client.ErrBucketNotEmpty) {
		t.Fatalf("DeleteBucket with an object: %v", err)
	}
	if _, err := mc.StatObject(bg, name, "keep/me.txt", minio.StatObjectOptions{}); err != nil {
		t.Errorf("the object is gone: %v", err)
	}
	// A delete marker is still something in the bucket.
	if err := mc.RemoveObject(bg, name, "keep/me.txt", minio.RemoveObjectOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteBucket(bg, name); !errors.Is(err, s3client.ErrBucketNotEmpty) {
		t.Fatalf("DeleteBucket with a delete marker and a version: %v", err)
	}
	if err := mc.RemoveObject(bg, name, "keep/me.txt", minio.RemoveObjectOptions{VersionID: info.VersionID}); err != nil {
		t.Fatal(err)
	}
	for o := range mc.ListObjects(bg, name, minio.ListObjectsOptions{Recursive: true, WithVersions: true}) {
		if o.Err != nil {
			t.Fatal(o.Err)
		}
		if err := mc.RemoveObject(bg, name, o.Key, minio.RemoveObjectOptions{VersionID: o.VersionID}); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.DeleteBucket(bg, name); err != nil {
		t.Fatalf("DeleteBucket of an empty bucket: %v", err)
	}
	if st, err := c.Bucket(bg, name); err != nil || st.Exists {
		t.Errorf("the bucket is still there: %+v, %v", st, err)
	}
	if err := c.DeleteBucket(bg, name); err != nil {
		t.Errorf("DeleteBucket of a bucket that is gone: %v", err)
	}
}

// The quota routes and bodies: a bucket without a quota answers 200 with
// quota 0 (a 404 is a bucket that is not there), the body of a set carries
// quota and quotatype, a type other than HARD is refused and a clear is a
// DELETE.
func TestQuotaRoutes(t *testing.T) {
	sv := load(t)
	name := unique("it-quota")
	sv.makeBucket(t, name, false)
	query := url.Values{"bucket": {name}}

	get := func() (int64, string) {
		t.Helper()
		status, body := sv.raw(t, http.MethodGet, "/get-bucket-quota", query, nil)
		if status != http.StatusOK {
			t.Fatalf("get-bucket-quota: HTTP %d: %s", status, body)
		}
		var q struct {
			Quota     int64  `json:"quota"`
			Quotatype string `json:"quotatype"`
		}
		if err := json.Unmarshal(body, &q); err != nil {
			t.Fatalf("get-bucket-quota: %v: %s", err, body)
		}
		return q.Quota, q.Quotatype
	}
	if quota, _ := get(); quota != 0 {
		t.Errorf("quota of a bucket without one = %d", quota)
	}

	if status, _ := sv.raw(t, http.MethodPut, "/set-bucket-quota", query, []byte(`{"quota":1048576,"quotatype":"FIFO"}`)); status != http.StatusBadRequest {
		t.Errorf("a quota of type FIFO: HTTP %d, want 400", status)
	}
	body := []byte(`{"quota":` + strconv.Itoa(1<<20) + `,"quotatype":"HARD"}`)
	eventually(t, 90*time.Second, "set-bucket-quota", func() error {
		status, out := sv.raw(t, http.MethodPut, "/set-bucket-quota", query, body)
		if status != http.StatusOK {
			return errors.New("HTTP " + strconv.Itoa(status) + ": " + string(out))
		}
		return nil
	})
	if quota, kind := get(); quota != 1<<20 || kind != "hard" {
		t.Errorf("quota = %d of type %q", quota, kind)
	}
	if status, out := sv.raw(t, http.MethodDelete, "/quota/"+name, nil, nil); status != http.StatusOK {
		t.Fatalf("clear: HTTP %d: %s", status, out)
	}
	if quota, _ := get(); quota != 0 {
		t.Errorf("quota after a clear = %d", quota)
	}
}
