package gcs

import (
	"context"
	"errors"

	"github.com/craftainer-labs/lib-operator-go/pkg/cloud"

	"github.com/craftainer-labs/operator-s3/internal/s3client"
)

// Probe checks that the project is usable: the credentials are valid and may
// list the project's buckets (storage.buckets.list, the smallest read of the
// service that proves the project exists and the identity may use Cloud
// Storage in it). The idle connections of c's HTTP client are closed on
// return, so a client built for one probe leaves nothing behind.
func Probe(ctx context.Context, c *cloud.GCPClient, project string) error {
	defer c.HTTPClient.CloseIdleConnections()
	s, err := New(ctx, c, project, "")
	if err != nil {
		return err
	}
	if _, err = s.ServerVersion(ctx); !errors.Is(err, s3client.ErrNoVersion) {
		return err
	}
	return nil
}
