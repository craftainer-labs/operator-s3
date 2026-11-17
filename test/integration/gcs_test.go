//go:build integration && gcs

package integration

// The GCS store of NKs3-2a (internal/gcs) and its probe against fake-gcs-server,
// reached at its container address (a private range, never loopback: pkg/egress
// blocks loopback in both modes) through the egress policy of
// EGRESS_MODE=allow-private in this test's own wiring only.
//
// What fake-gcs-server cannot check, and these tests therefore do not: it keeps
// a bucket's name and versioning only, so the ownership label, the lifecycle
// rules, the location and the IAM configuration (uniform bucket-level access,
// public access prevention) are accepted and dropped (the httptest fake of
// internal/gcs checks that the store sends them, not that GCS keeps them); it
// does not enforce IAM or validate a token, so a refused credential, the 403 of
// a bucket another project holds and the workload identity of a real cluster
// are not exercised; and it has no service accounts, bucket IAM policies or HMAC
// keys, which BucketAccess on GCP will need. They are checked by a real Google
// Cloud project, which no CI lane has.

import (
	"context"
	"errors"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/craftainer-labs/lib-operator-go/pkg/cloud"
	"github.com/craftainer-labs/lib-operator-go/pkg/egress"

	"github.com/craftainer-labs/operator-s3/internal/gcs"
	"github.com/craftainer-labs/operator-s3/internal/s3client"
)

// gcsEndpoint is the fake-gcs-server, from FAKE_GCS_ENDPOINT
// (http://<private ip>:4443).
func gcsEndpoint(t *testing.T) string {
	t.Helper()
	v := os.Getenv("FAKE_GCS_ENDPOINT")
	if v == "" {
		t.Fatal("FAKE_GCS_ENDPOINT is not set; see README.md here")
	}
	return v
}

// gcsClient is the client of a GCP store pointed at the emulator: its requests
// dial through the egress policy in allow-private mode, and carry no token
// (the emulator has none to check).
func gcsClient(t *testing.T) *cloud.GCPClient {
	t.Helper()
	dial, err := egress.NewPolicy(egress.ModeAllowPrivate).HostDialer(nil)
	if err != nil {
		t.Fatal(err)
	}
	hc := &http.Client{Transport: &http.Transport{DialContext: dial}}
	t.Cleanup(hc.CloseIdleConnections)
	return &cloud.GCPClient{HTTPClient: hc, Endpoint: gcsEndpoint(t)}
}

func gcsStore(t *testing.T) *gcs.Store {
	t.Helper()
	s, err := gcs.New(context.Background(), gcsClient(t), "test-project", "")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestGCSBucketLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s := gcsStore(t)
	name := unique("gcs")
	t.Cleanup(func() { _ = s.DeleteBucket(context.Background(), name) })

	if st, err := s.Bucket(ctx, name); err != nil || st.Exists {
		t.Fatalf("absent bucket: %+v, %v", st, err)
	}

	p := s3client.BucketParams{
		Name: name, Marker: s.EncodeMarker("ns/claim/uid"), Versioning: true,
		Rules: []s3client.Rule{{ID: "tmp", Prefix: "tmp/", Days: 7}},
	}
	if err := s.CreateBucket(ctx, p); err != nil {
		t.Fatalf("create: %v", err)
	}
	st, err := s.Bucket(ctx, name)
	if err != nil || !st.Exists || !st.Versioning {
		t.Fatalf("after create: %+v, %v", st, err)
	}
	if err := s.CreateBucket(ctx, p); !errors.Is(err, s3client.ErrBucketExists) {
		t.Errorf("second create: %v", err)
	}

	// Update: versioning is suspended.
	p.Versioning = false
	if err := s.UpdateBucket(ctx, p, st); err != nil {
		t.Fatalf("update: %v", err)
	}
	if st, err = s.Bucket(ctx, name); err != nil || st.Versioning {
		t.Errorf("after update: %+v, %v", st, err)
	}

	if err := s.DeleteBucket(ctx, name); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if st, err = s.Bucket(ctx, name); err != nil || st.Exists {
		t.Errorf("after delete: %+v, %v", st, err)
	}
	if err := s.DeleteBucket(ctx, name); err != nil {
		t.Errorf("delete of a deleted bucket: %v", err)
	}
}

// A quota and object lock are refused before any request.
func TestGCSUnsupportedParametersAreRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s := gcsStore(t)
	name := unique("gcs-refused")
	t.Cleanup(func() { _ = s.DeleteBucket(context.Background(), name) })

	if err := s.CreateBucket(ctx, s3client.BucketParams{Name: name, Marker: "m", Quota: 1 << 30}); !errors.Is(err, gcs.ErrQuotaUnsupported) {
		t.Errorf("quota: %v", err)
	}
	if err := s.CreateBucket(ctx, s3client.BucketParams{Name: name, Marker: "m", ObjectLock: true}); !errors.Is(err, gcs.ErrObjectLockUnsupported) {
		t.Errorf("object lock: %v", err)
	}
	if st, err := s.Bucket(ctx, name); err != nil || st.Exists {
		t.Errorf("a bucket was made: %+v, %v", st, err)
	}
}

// The probe lists the project's buckets.
func TestGCSProbe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := gcs.Probe(ctx, gcsClient(t), "test-project"); err != nil {
		t.Errorf("probe: %v", err)
	}
}
