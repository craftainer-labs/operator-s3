package awss3

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/craftainer-labs/lib-operator-go/pkg/lifecycle"

	"github.com/craftainer-labs/operator-s3/internal/s3client"
)

const marker = "ns/claim/uid"

func params(name string) s3client.BucketParams {
	return s3client.BucketParams{Name: name, Marker: marker}
}

// A bucket is created, read back and updated.
func TestBucketLifecycle(t *testing.T) {
	f := newFakeS3()
	s := New(f.serve(t, "us-east-1"))
	ctx := context.Background()

	st, err := s.Bucket(ctx, "b1")
	if err != nil || st.Exists {
		t.Fatalf("absent bucket: %+v, %v", st, err)
	}

	p := params("b1")
	p.Versioning, p.ObjectLock = true, true
	p.Rules = []s3client.Rule{{ID: "b", Prefix: "logs/", Days: 7}, {ID: "a", Prefix: "", Days: 30}}
	if err := s.CreateBucket(ctx, p); err != nil {
		t.Fatalf("create: %v", err)
	}
	if b := f.bucket("b1"); !b.lock || b.location != "" || b.versioning != "Enabled" || b.tags[s3client.MarkerTag] != marker || len(b.rules) != 2 {
		t.Errorf("created bucket = %+v", b)
	}

	st, err = s.Bucket(ctx, "b1")
	if err != nil || !st.Exists || st.Marker() != marker || !st.Versioning || st.Quota != 0 || !s3client.RulesEqual(st.Rules, p.Rules) {
		t.Fatalf("read: %+v, %v", st, err)
	}
	if st.Rules[0].ID != "a" {
		t.Errorf("rules are not sorted: %+v", st.Rules)
	}

	// An update turns versioning off and drops the rules.
	st, _ = s.Bucket(ctx, "b1")
	up := params("b1")
	if err := s.UpdateBucket(ctx, up, st); err != nil {
		t.Fatalf("update: %v", err)
	}
	b := f.bucket("b1")
	if b.versioning != "Suspended" || b.hasRules || b.tags[s3client.MarkerTag] != marker {
		t.Errorf("updated bucket = %+v", b)
	}
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()
	st, _ = s.Bucket(ctx, "b1")
	if err := s.UpdateBucket(ctx, up, st); err != nil || strings.Contains(f.callList(), "PUT") {
		t.Errorf("an up-to-date bucket was written: %v\n%s", err, f.callList())
	}

	// A second create is a collision, whoever made the bucket.
	if err := s.CreateBucket(ctx, p); !errors.Is(err, s3client.ErrBucketExists) {
		t.Errorf("second create: %v", err)
	}

	if err := s.DeleteBucket(ctx, "b1"); err != nil || f.bucket("b1") != nil {
		t.Errorf("delete: %v", err)
	}
	if err := s.DeleteBucket(ctx, "b1"); err != nil {
		t.Errorf("delete of a deleted bucket: %v", err)
	}
}

// A bucket outside us-east-1 is created with its location constraint.
func TestCreateInAnotherRegion(t *testing.T) {
	f := newFakeS3()
	s := New(f.serve(t, "eu-west-1"))
	if err := s.CreateBucket(context.Background(), params("b")); err != nil {
		t.Fatal(err)
	}
	if got := f.bucket("b").location; got != "eu-west-1" {
		t.Errorf("location = %q", got)
	}
}

// A quota is refused before anything is sent.
func TestQuotaIsRefused(t *testing.T) {
	f := newFakeS3()
	s := New(f.serve(t, "us-east-1"))
	p := params("b")
	p.Quota = 1 << 30
	if err := s.Validate(p); !errors.Is(err, ErrQuotaUnsupported) {
		t.Errorf("validate: %v", err)
	}
	if err := s.Validate(params("b")); err != nil {
		t.Errorf("validate without a quota: %v", err)
	}
	if err := s.CreateBucket(context.Background(), p); !errors.Is(err, ErrQuotaUnsupported) {
		t.Errorf("create: %v", err)
	}
	if err := s.UpdateBucket(context.Background(), p, s3client.BucketState{}); !errors.Is(err, ErrQuotaUnsupported) {
		t.Errorf("update: %v", err)
	}
	if got := f.callList(); got != "" {
		t.Errorf("requests were sent: %s", got)
	}
}

// A bucket that lives in another region is a collision, on every path that
// reads or creates. A 403 is an authentication failure, never a collision.
func TestForeignBucket(t *testing.T) {
	f0 := newFakeS3()
	f0.refuse["HEAD"] = refusal{http.StatusForbidden, "Forbidden"}
	s0 := New(f0.serve(t, "us-east-1"))
	if _, err := s0.Bucket(context.Background(), "b"); errors.Is(err, s3client.ErrBucketExists) || !errors.Is(err, s3client.ErrConnAuth) {
		t.Errorf("403 read: %v", err)
	}
	if err := s0.CreateBucket(context.Background(), params("b")); errors.Is(err, s3client.ErrBucketExists) || !errors.Is(err, s3client.ErrConnAuth) {
		t.Errorf("403 create: %v", err)
	}
	for _, status := range []int{http.StatusMovedPermanently} {
		f := newFakeS3()
		f.refuse["HEAD"] = refusal{status, "Forbidden"}
		s := New(f.serve(t, "us-east-1"))
		if _, err := s.Bucket(context.Background(), "b"); !errors.Is(err, s3client.ErrBucketExists) {
			t.Errorf("%d read: %v", status, err)
		}
		if err := s.CreateBucket(context.Background(), params("b")); !errors.Is(err, s3client.ErrBucketExists) {
			t.Errorf("%d create: %v", status, err)
		}
	}
	// Somebody made the bucket between the check and the create.
	f := newFakeS3()
	f.refuse["PUT"] = refusal{http.StatusConflict, "BucketAlreadyExists"}
	s := New(f.serve(t, "us-east-1"))
	if err := s.CreateBucket(context.Background(), params("b")); !errors.Is(err, s3client.ErrBucketExists) {
		t.Errorf("create race: %v", err)
	}
}

// A bucket that holds objects is not deleted.
func TestDeleteRefusesAFullBucket(t *testing.T) {
	f := newFakeS3()
	f.put("b", &fakeBucket{tags: map[string]string{}, objects: 1})
	s := New(f.serve(t, "us-east-1"))
	if err := s.DeleteBucket(context.Background(), "b"); !errors.Is(err, s3client.ErrBucketNotEmpty) {
		t.Errorf("delete: %v", err)
	}
	if f.bucket("b") == nil {
		t.Error("the bucket was deleted")
	}
}

// Every failure is one of the categories of package s3client, and the error
// carries neither the host nor the text of the answer.
func TestFailuresAreCategories(t *testing.T) {
	ctx := context.Background()
	bucket := func(s *Store) error { _, err := s.Bucket(ctx, "b"); return err }
	cases := []struct {
		name, key string
		call      func(*Store) error
	}{
		{"head", "HEAD", bucket},
		{"tagging", "GET tagging", bucket},
		{"versioning", "GET versioning", bucket},
		{"lifecycle", "GET lifecycle", bucket},
		{"create", "PUT", func(s *Store) error { return s.CreateBucket(ctx, params("new")) }},
		{"delete", "DELETE", func(s *Store) error { return s.DeleteBucket(ctx, "b") }},
		{"list", "GET", func(s *Store) error { _, err := s.ServerVersion(ctx); return err }},
	}
	for _, status := range []int{http.StatusInternalServerError, http.StatusForbidden} {
		for _, tc := range cases {
			f := newFakeS3()
			f.put("b", &fakeBucket{tags: map[string]string{}})
			f.refuse[tc.key] = refusal{status, "Boom"}
			err := tc.call(New(f.serve(t, "us-east-1")))
			var apiErr *s3client.Error
			code := "Boom"
			if tc.name == "head" {
				code = "" // a HEAD answer has no body, so no code
			}
			if err == nil || !errors.As(err, &apiErr) || apiErr.Status != status || apiErr.Code != code {
				t.Errorf("%s/%d: %v", tc.name, status, err)
				continue
			}
			if status == http.StatusForbidden && (!errors.Is(err, s3client.ErrConnAuth) || !errors.Is(err, lifecycle.ErrUnreachable)) {
				t.Errorf("%s/%d is not an authentication failure: %v", tc.name, status, err)
			}
			if strings.Contains(err.Error(), "secret detail") || strings.Contains(err.Error(), "127.0.0.1") {
				t.Errorf("%s/%d leaks: %v", tc.name, status, err)
			}
		}
	}
}

// What the claim does not own is kept when it takes a bucket over.
func TestUpdateKeepsForeignTags(t *testing.T) {
	f := newFakeS3()
	f.put("b", &fakeBucket{tags: map[string]string{"team": "x"}})
	s := New(f.serve(t, "us-east-1"))
	st, err := s.Bucket(context.Background(), "b")
	if err != nil || st.Marker() != "" {
		t.Fatalf("%+v, %v", st, err)
	}
	if err := s.UpdateBucket(context.Background(), params("b"), st); err != nil {
		t.Fatal(err)
	}
	if tags := f.bucket("b").tags; tags["team"] != "x" || tags[s3client.MarkerTag] != marker {
		t.Errorf("tags = %v", tags)
	}
}

// Failing calls after the first ones: every step of a write is reported.
func TestWriteStepsFail(t *testing.T) {
	ctx := context.Background()
	p := params("b")
	p.Versioning = true
	p.Rules = []s3client.Rule{{ID: "r", Days: 1}}
	for _, key := range []string{"PUT tagging", "PUT versioning", "PUT lifecycle"} {
		f := newFakeS3()
		f.put("b", &fakeBucket{tags: map[string]string{}})
		f.refuse[key] = refusal{http.StatusInternalServerError, "Boom"}
		s := New(f.serve(t, "us-east-1"))
		if err := s.UpdateBucket(ctx, p, s3client.BucketState{Exists: true}); err == nil {
			t.Errorf("%s: no error", key)
		}
	}
	f := newFakeS3()
	f.put("b", &fakeBucket{tags: map[string]string{}, hasRules: true})
	f.refuse["DELETE lifecycle"] = refusal{http.StatusInternalServerError, "Boom"}
	s := New(f.serve(t, "us-east-1"))
	cur := s3client.BucketState{Exists: true, Tags: map[string]string{s3client.MarkerTag: marker}, Rules: []s3client.Rule{{ID: "r", Days: 1}}}
	if err := s.UpdateBucket(ctx, params("b"), cur); err == nil {
		t.Error("DELETE lifecycle: no error")
	}
}

// A lifecycle rule the operator cannot express differs from every rule it
// writes, so it is replaced.
func TestForeignLifecycleRules(t *testing.T) {
	f := newFakeS3()
	f.put("b", &fakeBucket{tags: map[string]string{}})
	f.rawLifecycle = `<LifecycleConfiguration>
<Rule><ID>x</ID><Status>Enabled</Status><Filter><Tag><Key>k</Key><Value>v</Value></Tag></Filter><Expiration><Days>5</Days></Expiration></Rule>
<Rule><ID>y</ID><Status>Enabled</Status><Prefix>old/</Prefix><Expiration><Days>5</Days></Expiration></Rule>
<Rule><ID>z</ID><Status>Disabled</Status><Filter><Prefix>p/</Prefix></Filter><Expiration><Date>2030-01-01T00:00:00Z</Date></Expiration></Rule>
<Rule><ID>w</ID><Status>Disabled</Status><Filter><Prefix>q/</Prefix></Filter><Expiration><Days>9</Days></Expiration></Rule>
</LifecycleConfiguration>`
	s := New(f.serve(t, "us-east-1"))
	st, err := s.Bucket(context.Background(), "b")
	if err != nil || len(st.Rules) != 4 {
		t.Fatalf("%+v, %v", st, err)
	}
	for _, r := range st.Rules {
		if !r.Disabled {
			t.Errorf("rule %q is expressible: %+v", r.ID, r)
		}
	}
	if st.Rules[0].ID != "w" || st.Rules[0].Prefix != "q/" || st.Rules[0].Days != 9 {
		t.Errorf("a disabled rule of our shape keeps its fields: %+v", st.Rules[0])
	}
}

// A store that cannot be reached is an unreachable store, never the address.
func TestUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	cfg := testConfig(srv.URL, "us-east-1", srv.Client())
	srv.Close()
	var logged bytes.Buffer
	ctx := log.IntoContext(context.Background(), zap.New(zap.WriteTo(&logged), zap.UseDevMode(true)))
	_, err := New(cfg).Bucket(ctx, "b")
	if !errors.Is(err, lifecycle.ErrUnreachable) || !errors.Is(err, s3client.ErrConnFailed) {
		t.Fatalf("%v", err)
	}
	if strings.Contains(err.Error(), "127.0.0.1") || strings.Contains(logged.String(), "127.0.0.1") || logged.Len() == 0 {
		t.Errorf("error %q, log %q", err, logged.String())
	}
}

// ServerVersion proves the credentials and says S3 has no version.
func TestServerVersion(t *testing.T) {
	f := newFakeS3()
	s := New(f.serve(t, "us-east-1"))
	if v, err := s.ServerVersion(context.Background()); v != "" || !errors.Is(err, s3client.ErrNoVersion) {
		t.Errorf("%q, %v", v, err)
	}
}

// The IAM methods wait for NKs3-1b.
func TestIAMIsNotImplemented(t *testing.T) {
	s := New(newFakeS3().serve(t, "us-east-1"))
	ctx := context.Background()
	var errs []error
	_, err := s.User(ctx, "u")
	errs = append(errs, err)
	errs = append(errs, s.ApplyAccess(ctx, s3client.AccessParams{}, s3client.UserState{}))
	_, err = s.KeyExists(ctx, "k")
	errs = append(errs, err)
	errs = append(errs, s.IssueKey(ctx, "u", "k", "s"), s.DeleteKey(ctx, "k"), s.DeleteAccess(ctx, "u", nil))
	_, err = s.OwnedPolicies(ctx, "u")
	errs = append(errs, err)
	for i, err := range errs {
		if !errors.Is(err, ErrNotImplemented) {
			t.Errorf("method %d: %v", i, err)
		}
	}
}

// closer is an HTTP client that counts CloseIdleConnections.
type closer struct {
	http.Client
	closed int
}

func (c *closer) CloseIdleConnections() { c.closed++ }

func TestCloseIdleConnections(t *testing.T) {
	c := &closer{}
	cfg := testConfig("http://127.0.0.1:1", "us-east-1", nil)
	cfg.HTTPClient = c
	New(cfg).CloseIdleConnections()
	if c.closed != 1 {
		t.Errorf("closed %d times", c.closed)
	}
	// A client without the method is left alone.
	cfg.HTTPClient = nil
	New(cfg).CloseIdleConnections()
}

// AWS itself is addressed virtual-hosted: without an endpoint override the
// bucket is in the host name.
func TestVirtualHostedWithoutOverride(t *testing.T) {
	var host string
	hc := &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		host = r.URL.Host
		return nil, errors.New("stop")
	})}
	cfg := testConfig("", "eu-west-1", hc)
	cfg.BaseEndpoint = nil
	_, _ = New(cfg).Bucket(context.Background(), "mybucket")
	if host != "mybucket.s3.eu-west-1.amazonaws.com" {
		t.Errorf("host = %q", host)
	}
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
