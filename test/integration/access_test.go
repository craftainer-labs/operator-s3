//go:build integration

package integration

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"

	"github.com/craftainer-labs/operator-s3/internal/s3client"
)

// put writes an object as the key; get reads one; list lists a prefix.
func (s server) put(t *testing.T, key, secret, bucket, object string) error {
	t.Helper()
	_, err := s.minio(t, key, secret).PutObject(bg, bucket, object, bytes.NewReader([]byte("data")), 4, minio.PutObjectOptions{})
	return err
}

func (s server) get(t *testing.T, key, secret, bucket, object string) error {
	t.Helper()
	o, err := s.minio(t, key, secret).GetObject(bg, bucket, object, minio.GetObjectOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = o.Close() }()
	_, err = io.ReadAll(o)
	return err
}

func (s server) list(t *testing.T, key, secret, bucket, prefix string) error {
	t.Helper()
	for o := range s.minio(t, key, secret).ListObjects(bg, bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if o.Err != nil {
			return o.Err
		}
	}
	return nil
}

// denied tells whether err is the server refusing the key (403), as opposed to
// something else being wrong.
func denied(err error) bool {
	return err != nil && minio.ToErrorResponse(err).StatusCode == http.StatusForbidden
}

// A service account has the policy of its user and no more: the bucket and the
// prefix of the claim, read-only or read-write, and nothing in another bucket.
func TestPolicyConfinesAKeyToItsBucketAndPrefix(t *testing.T) {
	sv := load(t)
	c := sv.client(t)
	mine, theirs := unique("it-mine"), unique("it-theirs")
	admin := sv.makeBucket(t, mine, false)
	sv.makeBucket(t, theirs, false)
	for _, object := range []string{"team/seed.txt", "other/seed.txt"} {
		if _, err := admin.PutObject(bg, mine, object, bytes.NewReader([]byte("seed")), 4, minio.PutObjectOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := admin.PutObject(bg, theirs, "team/seed.txt", bytes.NewReader([]byte("seed")), 4, minio.PutObjectOptions{}); err != nil {
		t.Fatal(err)
	}

	// within runs fn until the policy shows (the server may take a moment to
	// apply a change of policy to a key).
	within := func(what string, fn func() error) {
		t.Helper()
		eventually(t, 15*time.Second, what, fn)
	}
	refused := func(err error) error {
		if !denied(err) {
			return errors.New("not refused with 403: " + errString(err))
		}
		return nil
	}

	// Read-write under team/ of the one bucket.
	user := unique("it-conf")
	grant(t, c, user, mine, "team/", true)
	key, secret := s3client.AccessKey(unique("uid"), 0), newSecret()
	if err := c.IssueKey(bg, user, key, secret); err != nil {
		t.Fatalf("IssueKey: %v", err)
	}
	within("write under the prefix", func() error { return sv.put(t, key, secret, mine, "team/new.txt") })
	within("read under the prefix", func() error { return sv.get(t, key, secret, mine, "team/seed.txt") })
	within("list the prefix", func() error { return sv.list(t, key, secret, mine, "team/") })
	within("write outside the prefix", func() error { return refused(sv.put(t, key, secret, mine, "other/new.txt")) })
	within("read outside the prefix", func() error { return refused(sv.get(t, key, secret, mine, "other/seed.txt")) })
	within("list outside the prefix", func() error { return refused(sv.list(t, key, secret, mine, "other/")) })
	within("list the whole bucket", func() error { return refused(sv.list(t, key, secret, mine, "")) })
	within("write in another bucket", func() error { return refused(sv.put(t, key, secret, theirs, "team/new.txt")) })
	within("read in another bucket", func() error { return refused(sv.get(t, key, secret, theirs, "team/seed.txt")) })
	within("list another bucket", func() error { return refused(sv.list(t, key, secret, theirs, "team/")) })

	// The policy changes, the key stays: read-only, then the whole bucket.
	st, err := c.User(bg, user)
	if err != nil {
		t.Fatal(err)
	}
	readOnly := access(user, mine, "team/", false)
	if err := c.ApplyAccess(bg, readOnly, st); err != nil {
		t.Fatalf("ApplyAccess: %v", err)
	}
	within("write after the change to read-only", func() error { return refused(sv.put(t, key, secret, mine, "team/again.txt")) })
	within("read after the change to read-only", func() error { return sv.get(t, key, secret, mine, "team/seed.txt") })
	within("delete after the change to read-only", func() error {
		err := sv.minio(t, key, secret).RemoveObject(bg, mine, "team/seed.txt", minio.RemoveObjectOptions{})
		return refused(err)
	})

	st, err = c.User(bg, user)
	if err != nil {
		t.Fatal(err)
	}
	whole := access(user, mine, "", true)
	if err := c.ApplyAccess(bg, whole, st); err != nil {
		t.Fatalf("ApplyAccess: %v", err)
	}
	within("write anywhere in the bucket", func() error { return sv.put(t, key, secret, mine, "other/new.txt") })
	within("list the whole bucket after the change", func() error { return sv.list(t, key, secret, mine, "") })
	within("write in another bucket after the change", func() error { return refused(sv.put(t, key, secret, theirs, "team/new.txt")) })
}

func errString(err error) string {
	if err == nil {
		return "no error"
	}
	return err.Error()
}
