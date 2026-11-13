package controller

import (
	"context"
	"errors"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/craftainer-labs/lib-operator-go/pkg/probe"
	"github.com/craftainer-labs/lib-operator-go/pkg/server"

	s3v1alpha1 "github.com/craftainer-labs/operator-s3/api/v1alpha1"
	"github.com/craftainer-labs/operator-s3/internal/s3client"
)

// Prober is the probe.Prober of both store kinds: it builds a client through
// the egress policy and the store's proxy and TLS settings (the same target
// the claims' reconcilers use) and reads the server version from the admin
// API, an authenticated call that proves the store is reachable and accepts
// the admin credentials. The caller (probe.Reconciler) bounds it by
// Options.Timeout and shows only that it failed; the error text names a
// category of failure, never a key or a host.
type Prober struct{ Config Config }

var _ probe.Prober = Prober{}

// Probe returns the server version of srv. A store that answers the
// authenticated call but names no version is healthy with an empty version
// (RustFS fills the field today, but reachability and accepted credentials are
// what the status promises). The client is built for the probe and its
// connections are closed with it, not taken from the claims' store cache, so a
// probe of a store nobody claims leaves nothing behind.
func (p Prober) Probe(ctx context.Context, srv server.Server, conn server.Connection) (string, error) {
	spec, err := storeConfigOf(srv)
	if err != nil {
		return "", err
	}
	dial, err := p.Config.Egress.HostDialer(conn.Proxy)
	if err != nil {
		return "", fmt.Errorf("%w: %w", server.ErrInvalidServer, err)
	}
	target, err := adminTarget(spec, conn, dial)
	if err != nil {
		return "", err
	}
	store, err := p.Config.NewStore(target)
	if err != nil {
		return "", fmt.Errorf("%w: %w", server.ErrInvalidServer, err)
	}
	if c, ok := store.(interface{ CloseIdleConnections() }); ok {
		defer c.CloseIdleConnections()
	}
	// The client logs the raw transport error (it names the host) and a
	// snippet of the answer at every call. The probe repeats every interval and
	// the reconciler logs its category once per change, so the client's own
	// log lines are dropped here.
	version, err := store.ServerVersion(log.IntoContext(ctx, log.FromContext(ctx).WithSink(nil)))
	if errors.Is(err, s3client.ErrNoVersion) {
		return "", nil
	}
	return version, err
}

// storeConfigOf returns the S3 fields of a store kind of this operator.
func storeConfigOf(srv server.Server) (s3v1alpha1.ObjectStoreConfig, error) {
	switch s := srv.(type) {
	case *s3v1alpha1.ClusterObjectStore:
		return s.Spec.ObjectStoreConfig, nil
	case *s3v1alpha1.ObjectStore:
		return s.Spec.ObjectStoreConfig, nil
	}
	return s3v1alpha1.ObjectStoreConfig{}, fmt.Errorf("%w: %T", server.ErrUnknownServerKind, srv)
}
