package gcs

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/craftainer-labs/lib-operator-go/pkg/cloud"
	"github.com/craftainer-labs/lib-operator-go/pkg/lifecycle"

	"golang.org/x/oauth2"

	"github.com/craftainer-labs/operator-s3/internal/s3client"
)

const marker = "ns/claim/uid"

func newStore(t *testing.T, f *fakeGCS) *Store {
	t.Helper()
	s, err := New(context.Background(), f.serve(t), "proj-12345", "eu")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.CloseIdleConnections)
	return s
}

func params(s *Store, name string) s3client.BucketParams {
	return s3client.BucketParams{Name: name, Marker: s.EncodeMarker(marker)}
}

// A marker is a digest that a label value can hold.
func TestEncodeMarker(t *testing.T) {
	s := &Store{}
	a, b := s.EncodeMarker(marker), s.EncodeMarker("ns/other/uid")
	if a == b || a != s.EncodeMarker(marker) {
		t.Errorf("not a function of the marker: %q %q", a, b)
	}
	if len(a) > 63 || strings.Trim(a, "0123456789abcdef") != "" {
		t.Errorf("%q is not a label value", a)
	}
}

// A bucket is created, read back, updated and deleted.
func TestBucketLifecycle(t *testing.T) {
	f := newFakeGCS()
	s := newStore(t, f)
	ctx := context.Background()

	if st, err := s.Bucket(ctx, "b1"); err != nil || st.Exists {
		t.Fatalf("absent bucket: %+v, %v", st, err)
	}

	p := params(s, "b1")
	p.Versioning = true
	p.Rules = []s3client.Rule{{ID: "b", Prefix: "logs/", Days: 7}, {ID: "a", Prefix: "", Days: 30}}
	if err := s.CreateBucket(ctx, p); err != nil {
		t.Fatalf("create: %v", err)
	}
	b := f.bucket("b1")
	if b["location"] != "eu" || b["labels"].(map[string]any)[LabelMarker] != p.Marker || f.lastProject() != "proj-12345" {
		t.Errorf("created bucket = %+v (project %q)", b, f.lastProject())
	}

	st, err := s.Bucket(ctx, "b1")
	if err != nil || !st.Exists || st.Marker() != p.Marker || !st.Versioning || st.Quota != 0 || !st.NoRuleIDs || !s3client.RulesMatch(st, p.Rules) {
		t.Fatalf("read: %+v, %v", st, err)
	}
	if st.Rules[0].Prefix != "" || st.Rules[1].Prefix != "logs/" || st.Rules[0].Disabled || st.Rules[1].Disabled {
		t.Errorf("rules: %+v", st.Rules)
	}

	// Another label stays; the update turns versioning off and drops the rules.
	f.setLabel("b1", "team", "a")
	st, _ = s.Bucket(ctx, "b1")
	if st.Tags["team"] != "a" || st.Tags[LabelMarker] != "" {
		t.Errorf("tags: %v", st.Tags)
	}
	up := params(s, "b1")
	if err := s.UpdateBucket(ctx, up, st); err != nil {
		t.Fatalf("update: %v", err)
	}
	b = f.bucket("b1")
	if b["versioning"].(map[string]any)["enabled"] != false || b["labels"].(map[string]any)["team"] != "a" {
		t.Errorf("updated bucket = %+v", b)
	}
	if st, _ = s.Bucket(ctx, "b1"); st.Versioning || len(st.Rules) != 0 {
		t.Errorf("after the update: %+v", st)
	}

	// A bucket with another marker gets this claim's.
	other := st
	other.Tags = map[string]string{s3client.MarkerTag: "other"}
	if err := s.UpdateBucket(ctx, up, other); err != nil || f.bucket("b1")["labels"].(map[string]any)[LabelMarker] != up.Marker {
		t.Errorf("marker update: %v", err)
	}

	// An up-to-date bucket is not written.
	st, _ = s.Bucket(ctx, "b1")
	f.reset()
	if err := s.UpdateBucket(ctx, up, st); err != nil || strings.Contains(f.callList(), "PATCH") {
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

// A bucket without a location asks for GCS's default.
func TestCreateWithoutLocation(t *testing.T) {
	f := newFakeGCS()
	s, err := New(context.Background(), f.serve(t), "proj-12345", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateBucket(context.Background(), params(s, "b")); err != nil {
		t.Fatal(err)
	}
	if loc, ok := f.bucket("b")["location"]; ok && loc != "" && loc != "US" { // the library asks for US when none is set
		t.Errorf("location = %v", loc)
	}
}

// A quota and object lock are refused before anything is sent.
func TestUnsupportedParametersAreRefused(t *testing.T) {
	f := newFakeGCS()
	s := newStore(t, f)
	quota, lock := params(s, "b"), params(s, "b")
	quota.Quota = 1 << 30
	lock.ObjectLock = true
	for p, want := range map[*s3client.BucketParams]error{&quota: ErrQuotaUnsupported, &lock: ErrObjectLockUnsupported} {
		if err := s.Validate(*p); !errors.Is(err, want) {
			t.Errorf("validate: %v, want %v", err, want)
		}
		if err := s.CreateBucket(context.Background(), *p); !errors.Is(err, want) {
			t.Errorf("create: %v, want %v", err, want)
		}
		if err := s.UpdateBucket(context.Background(), *p, s3client.BucketState{}); !errors.Is(err, want) {
			t.Errorf("update: %v, want %v", err, want)
		}
	}
	if err := s.Validate(params(s, "b")); err != nil {
		t.Errorf("validate without either: %v", err)
	}
	if got := f.callList(); got != "" {
		t.Errorf("requests were sent: %s", got)
	}
}

// A bucket that holds objects is not deleted.
func TestDeleteRefusesAFullBucket(t *testing.T) {
	f := newFakeGCS()
	f.buckets["b"] = map[string]any{"name": "b"}
	f.objects["b"] = true
	s := newStore(t, f)
	if err := s.DeleteBucket(context.Background(), "b"); !errors.Is(err, s3client.ErrBucketNotEmpty) {
		t.Errorf("delete: %v", err)
	}
	if f.bucket("b") == nil {
		t.Error("the bucket was deleted")
	}
}

// A name another project holds answers 409 on create, and a refusal of the
// credentials is an authentication failure, never a collision.
func TestForeignBucket(t *testing.T) {
	f := newFakeGCS()
	f.refuse["POST"] = http.StatusConflict
	s := newStore(t, f)
	if err := s.CreateBucket(context.Background(), params(s, "b")); !errors.Is(err, s3client.ErrBucketExists) {
		t.Errorf("create race: %v", err)
	}
	// A 403 on the bucket while the credentials can list the project's buckets
	// is a name held elsewhere: a collision, which a delete can get past.
	f = newFakeGCS()
	f.refuse["GET"] = http.StatusForbidden
	s = newStore(t, f)
	if _, err := s.Bucket(context.Background(), "b"); !errors.Is(err, s3client.ErrBucketExists) {
		t.Errorf("403 read with a working list: %v", err)
	}
	// A 403 on the list as well is a refused credential.
	f.refuse["GET list"] = http.StatusForbidden
	if _, err := s.Bucket(context.Background(), "b"); errors.Is(err, s3client.ErrBucketExists) || !errors.Is(err, s3client.ErrConnAuth) {
		t.Errorf("403 read: %v", err)
	}
}

// A refused token (no HTTP status of the API) is an authentication failure, and
// a client the library cannot build is a fixed error without the endpoint.
func TestTokenAndClientErrors(t *testing.T) {
	refused := &url.Error{Op: "Get", URL: "https://secret.example/token", Err: &oauth2.RetrieveError{
		Response: &http.Response{StatusCode: http.StatusBadRequest, Status: "400 Bad Request"}, Body: []byte("secret detail"),
	}}
	hc := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, refused })}
	s, err := New(context.Background(), &cloud.GCPClient{HTTPClient: hc, Endpoint: "http://gcs.invalid"}, "proj-12345", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Bucket(context.Background(), "b")
	if !errors.Is(err, s3client.ErrConnAuth) || strings.Contains(err.Error(), "secret") {
		t.Errorf("token refusal: %v", err)
	}

	_, err = New(context.Background(), &cloud.GCPClient{HTTPClient: hc, Endpoint: "http://[::1"}, "proj-12345", "")
	if !errors.Is(err, ErrClientNotBuilt) || strings.Contains(err.Error(), "::1") {
		t.Errorf("bad endpoint: %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// The store knows GCS rules have no ID, whatever state the caller passes.
func TestUpdateIgnoresCallerRuleIDs(t *testing.T) {
	f := newFakeGCS()
	s := newStore(t, f)
	p := params(s, "b1")
	p.Rules = []s3client.Rule{{ID: "x", Prefix: "p/", Days: 3}}
	if err := s.CreateBucket(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Bucket(context.Background(), "b1")
	st.NoRuleIDs = false
	f.reset()
	if err := s.UpdateBucket(context.Background(), p, st); err != nil || strings.Contains(f.callList(), "PATCH") {
		t.Errorf("written: %v\n%s", err, f.callList())
	}
}

// Every failure is one of the categories of package s3client, and the error
// carries neither the host nor the text of the answer.
func TestFailuresAreCategories(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name, key string
		call      func(*Store) error
	}{
		{"get", "GET", func(s *Store) error { _, err := s.Bucket(ctx, "b"); return err }},
		{"create", "POST", func(s *Store) error { return s.CreateBucket(ctx, params(s, "new")) }},
		{"update", "PATCH", func(s *Store) error {
			p := params(s, "b")
			p.Versioning = true
			return s.UpdateBucket(ctx, p, s3client.BucketState{})
		}},
		{"delete", "DELETE", func(s *Store) error { return s.DeleteBucket(ctx, "b") }},
		{"list", "GET list", func(s *Store) error { _, err := s.ServerVersion(ctx); return err }},
	}
	for _, status := range []int{http.StatusInternalServerError, http.StatusForbidden} {
		for _, tc := range cases {
			f := newFakeGCS()
			f.buckets["b"] = map[string]any{"name": "b"}
			f.refuse[tc.key] = status
			if tc.key == "GET" {
				f.refuse["GET list"] = status // else a 403 on the bucket is a foreign name
			}
			err := tc.call(newStore(t, f))
			var apiErr *s3client.Error
			if err == nil || !errors.As(err, &apiErr) || apiErr.Status != status || apiErr.Code != "boom" {
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

// A request that gets no answer is a connection failure.
func TestUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	c := &cloud.GCPClient{HTTPClient: srv.Client(), Endpoint: srv.URL}
	srv.Close()
	s, err := New(context.Background(), c, "proj-12345", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Bucket(context.Background(), "b")
	var apiErr *s3client.Error
	if err == nil || errors.As(err, &apiErr) || !errors.Is(err, lifecycle.ErrUnreachable) {
		t.Errorf("error = %v", err)
	}
	if strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("the error names the host: %v", err)
	}
}

// Lifecycle rules of a shape the operator cannot express are Disabled, so they
// are replaced.
func TestRuleShapes(t *testing.T) {
	cases := map[string]struct {
		rule     string
		disabled bool
		prefix   string
		days     int32
	}{
		"plain":       {`{"action":{"type":"Delete"},"condition":{"age":7}}`, false, "", 7},
		"prefix":      {`{"action":{"type":"Delete"},"condition":{"age":7,"matchesPrefix":["a/"]}}`, false, "a/", 7},
		"two":         {`{"action":{"type":"Delete"},"condition":{"age":7,"matchesPrefix":["a/","b/"]}}`, true, "", 7},
		"storage":     {`{"action":{"type":"SetStorageClass","storageClass":"COLDLINE"},"condition":{"age":7}}`, true, "", 7},
		"suffix":      {`{"action":{"type":"Delete"},"condition":{"age":7,"matchesSuffix":[".tmp"]}}`, true, "", 7},
		"no age":      {`{"action":{"type":"Delete"},"condition":{"numNewerVersions":2}}`, true, "", 0},
		"archived":    {`{"action":{"type":"Delete"},"condition":{"age":7,"isLive":false}}`, true, "", 7},
		"huge number": {`{"action":{"type":"Delete"},"condition":{"age":99999999999}}`, false, "", 1<<31 - 1},
	}
	for name, tc := range cases {
		f := newFakeGCS()
		raw := `{"name":"b","lifecycle":{"rule":[` + tc.rule + `]}}`
		f.buckets["b"] = decode(t, raw)
		st, err := newStore(t, f).Bucket(context.Background(), "b")
		if err != nil || len(st.Rules) != 1 {
			t.Fatalf("%s: %+v, %v", name, st, err)
		}
		if r := st.Rules[0]; r.Disabled != tc.disabled || r.Prefix != tc.prefix || r.Days != tc.days {
			t.Errorf("%s: %+v", name, r)
		}
	}
}

// The access methods of a GCP store: nothing is found or deleted, and nothing
// can be created.
func TestAccessIsNotImplemented(t *testing.T) {
	s := &Store{}
	ctx := context.Background()
	if us, err := s.User(ctx, "u"); err != nil || us.Exists {
		t.Errorf("User: %+v, %v", us, err)
	}
	if err := s.ApplyAccess(ctx, s3client.AccessParams{}, s3client.UserState{}); !errors.Is(err, ErrAccessNotImplemented) {
		t.Errorf("ApplyAccess: %v", err)
	}
	if owned, err := s.OwnedPolicies(ctx, "u"); err != nil || len(owned) != 0 {
		t.Errorf("OwnedPolicies: %v, %v", owned, err)
	}
	if err := s.DeleteAccess(ctx, "u", nil); err != nil {
		t.Errorf("DeleteAccess: %v", err)
	}
}

// A client the library cannot build is an error, and an endpoint override is
// where the API is.
func TestNewErrors(t *testing.T) {
	t.Setenv("STORAGE_EMULATOR_HOST", "http://[::1")
	if _, err := New(context.Background(), &cloud.GCPClient{HTTPClient: http.DefaultClient}, "p", ""); err == nil {
		t.Error("an unusable emulator host was accepted")
	}
}
