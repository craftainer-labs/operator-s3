package gcs

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/craftainer-labs/lib-operator-go/pkg/cloud"

	"github.com/craftainer-labs/operator-s3/internal/s3client"
)

// The probe lists the project's buckets, and a store has no version.
func TestProbe(t *testing.T) {
	f := newFakeGCS()
	f.buckets["b"] = map[string]any{"name": "b"}
	c := f.serve(t)
	if err := Probe(context.Background(), c, "proj-12345"); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if got := f.lastProject(); got != "proj-12345" {
		t.Errorf("project = %q", got)
	}
	s, err := New(context.Background(), c, "proj-12345", "")
	if err != nil {
		t.Fatal(err)
	}
	if v, err := s.ServerVersion(context.Background()); v != "" || !errors.Is(err, s3client.ErrNoVersion) {
		t.Errorf("version = %q, %v", v, err)
	}
	// A project with no bucket is healthy too.
	if err := Probe(context.Background(), newFakeGCS().serve(t), "proj-12345"); err != nil {
		t.Errorf("empty project: %v", err)
	}
}

// A refusal of the credentials and a client that cannot be built are errors.
func TestProbeFailures(t *testing.T) {
	f := newFakeGCS()
	f.refuse["GET list"] = http.StatusForbidden
	err := Probe(context.Background(), f.serve(t), "proj-12345")
	if !errors.Is(err, s3client.ErrConnAuth) {
		t.Errorf("403: %v", err)
	}
	t.Setenv("STORAGE_EMULATOR_HOST", "http://[::1")
	if err := Probe(context.Background(), &cloud.GCPClient{HTTPClient: http.DefaultClient}, "p"); err == nil {
		t.Error("an unusable client was accepted")
	}
}
