//go:build integration

package integration

import (
	"bytes"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/tags"

	"github.com/craftainer-labs/operator-s3/internal/s3client"
)

// The Bucket controller's calls in the order it makes them, against the
// server: observe, create, tell a bucket that is not the claim's from the
// claim's own, take over a bucket whose create stopped before the marker, and
// delete only an empty one.
func TestBucketClaimFlow(t *testing.T) {
	sv := load(t)
	c := sv.client(t)
	ns, claim := "it-ns", unique("claim")
	name := s3client.BucketName(ns, claim)
	marker := ns + "/" + claim + "/uid-1"
	sv.dropAfter(t, name)

	// Observe finds nothing, Create makes the bucket, Observe finds it up to
	// date.
	if st, err := c.Bucket(bg, name); err != nil || st.Exists {
		t.Fatalf("before the create: %+v, %v", st, err)
	}
	p := s3client.BucketParams{
		Name: name, Marker: marker, Versioning: true, Rules: []s3client.Rule{{ID: "expire", Prefix: "tmp/", Days: 1}},
	}
	create(t, c, p)
	st, err := c.Bucket(bg, name)
	if err != nil || !st.Exists || st.Marker() != marker || !st.Versioning || !s3client.RulesEqual(st.Rules, p.Rules) {
		t.Fatalf("after the create: %+v, %v", st, err)
	}

	// A bucket of the derived name that somebody else made has no marker (the
	// controller refuses it), and one a different claim made has that claim's.
	foreign := s3client.BucketName(ns, unique("foreign"))
	mc := sv.makeBucket(t, foreign, false)
	if st, err := c.Bucket(bg, foreign); err != nil || !st.Exists || st.Marker() != "" {
		t.Errorf("a bucket without a marker = %+v, %v", st, err)
	}
	rival := s3client.BucketName(ns, unique("rival"))
	sv.makeBucket(t, rival, false)
	theirs, err := tags.NewTags(map[string]string{s3client.MarkerTag: ns + "/rival/uid-9"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := mc.SetBucketTagging(bg, rival, theirs); err != nil {
		t.Fatal(err)
	}
	if st, err := c.Bucket(bg, rival); err != nil || st.Marker() != ns+"/rival/uid-9" {
		t.Errorf("a bucket of another claim = %+v, %v", st, err)
	}

	// The claim whose create stopped before the marker takes its bucket over:
	// UpdateBucket on what Bucket found writes the marker and the rest.
	crashed := s3client.BucketName(ns, unique("crashed"))
	sv.makeBucket(t, crashed, false)
	cp := s3client.BucketParams{Name: crashed, Marker: ns + "/crashed/uid-2", Versioning: true}
	st, err = c.Bucket(bg, crashed)
	if err != nil || !st.Exists || st.Marker() != "" {
		t.Fatalf("the crashed bucket = %+v, %v", st, err)
	}
	if err := c.UpdateBucket(bg, cp, st); err != nil {
		t.Fatalf("take over: %v", err)
	}
	if st, err := c.Bucket(bg, crashed); err != nil || st.Marker() != cp.Marker || !st.Versioning {
		t.Errorf("after the take-over = %+v, %v", st, err)
	}

	// Delete: objects keep the bucket (and stay), an empty bucket goes.
	admin := sv.admin(t)
	info, err := admin.PutObject(bg, name, "tmp/data.txt", bytes.NewReader([]byte("data")), 4, minio.PutObjectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteBucket(bg, name); !errors.Is(err, s3client.ErrBucketNotEmpty) {
		t.Fatalf("delete with an object: %v", err)
	}
	if _, err := admin.StatObject(bg, name, "tmp/data.txt", minio.StatObjectOptions{}); err != nil {
		t.Errorf("the operator deleted an object: %v", err)
	}
	if err := admin.RemoveObject(bg, name, "tmp/data.txt", minio.RemoveObjectOptions{VersionID: info.VersionID}); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteBucket(bg, name); err != nil {
		t.Fatalf("delete of the empty bucket: %v", err)
	}
	if st, err := c.Bucket(bg, name); err != nil || st.Exists {
		t.Errorf("after the delete: %+v, %v", st, err)
	}
}

// The BucketAccess controller's calls in the order it makes them: create the
// user with its policy and the first key, observe, change the spec without
// touching the key, rotate with an overlap, and delete everything the claim
// derived while the bucket and its objects stay.
func TestAccessClaimFlow(t *testing.T) {
	sv := load(t)
	c := sv.client(t)
	ns, claim := "it-ns", unique("access")
	bucket := unique("it-flow")
	admin := sv.makeBucket(t, bucket, false)
	for _, object := range []string{"p/seed.txt", "q/seed.txt"} {
		if _, err := admin.PutObject(bg, bucket, object, bytes.NewReader([]byte("seed")), 4, minio.PutObjectOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	user := s3client.UserName(ns, claim)
	uid := unique("uid")
	key := func(seq int64) string { return s3client.AccessKey(uid, seq) }
	t.Cleanup(func() {
		if err := c.DeleteAccess(bg, user, []string{key(0), key(1), key(2)}); err != nil {
			t.Logf("cleanup: %v", err)
		}
	})
	reads := func(what, ak, secret, object string, ok bool) {
		t.Helper()
		eventually(t, 15*time.Second, what, func() error {
			err := sv.get(t, ak, secret, bucket, object)
			switch {
			case ok && err != nil:
				return err
			case !ok && !denied(err):
				return errors.New("not refused with 403: " + errString(err))
			}
			return nil
		})
	}

	// Create: the user, its policy, the first generation's key.
	first := access(user, bucket, "p/", true)
	st, err := c.User(bg, user)
	if err != nil || st.Exists {
		t.Fatalf("before the create: %+v, %v", st, err)
	}
	if err := c.ApplyAccess(bg, first, s3client.UserState{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	s0 := newSecret()
	if err := c.IssueKey(bg, user, key(0), s0); err != nil {
		t.Fatalf("IssueKey: %v", err)
	}

	// Observe: up to date when the user is enabled with exactly the policy and
	// the active key exists.
	st, err = c.User(bg, user)
	if err != nil || !st.Exists || !st.Enabled || !slices.Equal(st.Policies, []string{first.Policy}) {
		t.Fatalf("observe: %+v, %v", st, err)
	}
	if ok, err := c.KeyExists(bg, key(0)); err != nil || !ok {
		t.Fatalf("the active key: %v, %v", ok, err)
	}
	reads("the key reads its prefix", key(0), s0, "p/seed.txt", true)
	reads("the key does not read the rest", key(0), s0, "q/seed.txt", false)

	// Update: another prefix; the key and its secret stay.
	second := access(user, bucket, "q/", false)
	if err := c.ApplyAccess(bg, second, st); err != nil {
		t.Fatalf("update: %v", err)
	}
	reads("the same key reads the new prefix", key(0), s0, "q/seed.txt", true)
	reads("the same key does not read the old one", key(0), s0, "p/seed.txt", false)

	// Rotate: the next key first, the previous one after the grace period.
	s1 := newSecret()
	if err := c.IssueKey(bg, user, key(1), s1); err != nil {
		t.Fatalf("RotateBegin: %v", err)
	}
	reads("the next key", key(1), s1, "q/seed.txt", true)
	reads("the previous key during the grace period", key(0), s0, "q/seed.txt", true)
	if err := c.DeleteKey(bg, key(0)); err != nil {
		t.Fatalf("RotateFinish: %v", err)
	}
	reads("the next key after the previous one is gone", key(1), s1, "q/seed.txt", true)
	eventually(t, 15*time.Second, "the previous key is refused", func() error {
		if err := sv.get(t, key(0), s0, bucket, "q/seed.txt"); err == nil {
			return errors.New("still accepted")
		}
		return nil
	})

	// Delete: the keys the claim derived, the user and its policy.
	if err := c.DeleteAccess(bg, user, []string{key(0), key(1), key(2)}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if st, err := c.User(bg, user); err != nil || st.Exists {
		t.Errorf("the user after the delete: %+v, %v", st, err)
	}
	for seq := range int64(3) {
		if ok, err := c.KeyExists(bg, key(seq)); err != nil || ok {
			t.Errorf("key %d after the delete: %v, %v", seq, ok, err)
		}
	}
	if owned, err := c.OwnedPolicies(bg, user); err != nil || len(owned) != 0 {
		t.Errorf("policies after the delete: %v, %v", owned, err)
	}
	for _, object := range []string{"p/seed.txt", "q/seed.txt"} {
		if _, err := admin.StatObject(bg, bucket, object, minio.StatObjectOptions{}); err != nil {
			t.Errorf("object %s after the delete: %v", object, err)
		}
	}
	if st, err := c.Bucket(bg, bucket); err != nil || !st.Exists {
		t.Errorf("the bucket after the delete: %+v, %v", st, err)
	}
}
