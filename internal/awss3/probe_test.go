package awss3

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/craftainer-labs/operator-s3/internal/s3client"
)

// The probe asks STS who the caller is and S3 for the account's buckets, and
// closes the connections it opened.
func TestProbe(t *testing.T) {
	f := newFakeS3()
	c := &closer{}
	cfg := f.serve(t, "us-east-1")
	cfg.HTTPClient = c
	// closer wraps a client that reaches the fake.
	c.Client = *http.DefaultClient
	account, err := Probe(context.Background(), cfg)
	if err != nil || account != "123456789012" {
		t.Fatalf("%q, %v", account, err)
	}
	if got := f.callList(); got != "STS\nGET /?" {
		t.Errorf("calls:\n%s", got)
	}
	if c.closed != 1 {
		t.Errorf("closed %d times", c.closed)
	}
}

// A failure of either call is a category, and the second is not made after the
// first fails.
func TestProbeFailures(t *testing.T) {
	f := newFakeS3()
	f.sts = http.StatusForbidden
	_, err := Probe(context.Background(), f.serve(t, "us-east-1"))
	var apiErr *s3client.Error
	if !errors.Is(err, s3client.ErrConnAuth) || !errors.As(err, &apiErr) || strings.Contains(f.callList(), "GET") {
		t.Errorf("sts: %v\n%s", err, f.callList())
	}

	f = newFakeS3()
	f.refuse["GET"] = refusal{http.StatusForbidden, "AccessDenied"}
	_, err = Probe(context.Background(), f.serve(t, "us-east-1"))
	if !errors.Is(err, s3client.ErrConnAuth) {
		t.Errorf("s3: %v", err)
	}
}
