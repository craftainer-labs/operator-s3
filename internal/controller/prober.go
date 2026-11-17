package controller

import (
	"context"
	"errors"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/craftainer-labs/lib-operator-go/pkg/cloud"
	"github.com/craftainer-labs/lib-operator-go/pkg/probe"
	"github.com/craftainer-labs/lib-operator-go/pkg/server"

	s3v1alpha1 "github.com/craftainer-labs/operator-s3/api/v1alpha1"
	"github.com/craftainer-labs/operator-s3/internal/s3client"
)

// Prober is the probe.CloudProber of both store kinds: it builds a client through
// the egress policy and the store's proxy and TLS settings (the same target
// the claims' reconcilers use) and reads the server version from the admin
// API, an authenticated call that proves the store is reachable and accepts
// the admin credentials. The caller (probe.Reconciler) bounds it by
// Options.Timeout and shows only that it failed; the error text names a
// category of failure, never a key or a host.
type Prober struct{ Config Config }

var _ probe.CloudProber = Prober{}

// ProbeCloud returns the server version of srv. A store that answers the
// authenticated call but names no version is healthy with an empty version
// (RustFS fills the field today, but reachability and accepted credentials are
// what the status promises). The client is built for the probe and its
// connections are closed with it, not taken from the claims' store cache, so a
// probe of a store nobody claims leaves nothing behind.
//
// A store on AWS is probed by probeAWS and one on GCP by probeGCP; they report
// what they discovered.
func (p Prober) ProbeCloud(ctx context.Context, srv server.CloudServer, cc server.CloudConnection) (probe.CloudResult, error) {
	spec, cs, err := storeConfigOf(srv)
	if err != nil {
		return probe.CloudResult{}, err
	}
	if cs.set && cs.kind == cloud.ProviderGCP {
		return p.probeGCP(ctx, cs, cc)
	}
	if cs.set {
		return p.probeAWS(ctx, cs, cc)
	}
	version, err := p.probe(ctx, spec, cc.Connection)
	return probe.CloudResult{Version: version}, err
}

// Keys of ServerStatus.Discovered an AWS store writes (ADR 0002, section 16).
// The endpoint is the scheme and host only, whatever an endpointOverride holds.
const (
	DiscoveredRegion    = "region"
	DiscoveredEndpoint  = "endpoint"
	DiscoveredAccountID = "accountId"
)

// probeAWS builds the store's AWS configuration (which proves the credentials
// can be had, and refuses an endpointOverride the operator does not allow) and
// asks STS and S3 for the caller and the buckets. Its errors are not
// server.ErrInvalidServer on purpose: the probe shows any failure as
// Ready=False, ServerUnreachable with a fixed text, and the reason of a
// refused override or a missing target is a known gap of lib-operator-go
// v0.6.0 (NKlib-3). The version stays empty: S3 has none.
func (p Prober) probeAWS(ctx context.Context, cs cloudStore, cc server.CloudConnection) (probe.CloudResult, error) {
	// The SDK logs through the context's logger; the probe repeats every
	// interval and the reconciler logs its failure once per change.
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithSink(nil))
	cfg, err := p.Config.awsConfig(ctx, cs, cc)
	if err != nil {
		return probe.CloudResult{}, err
	}
	account, err := p.Config.ProbeAWS(ctx, cfg)
	if err != nil {
		return probe.CloudResult{}, err
	}
	return probe.CloudResult{Discovered: map[string]string{
		DiscoveredRegion: cs.aws.Region, DiscoveredEndpoint: bareEndpoint(awsEndpoint(cs.aws)), DiscoveredAccountID: account,
	}}, nil
}

// probeGCP builds the store's authorized client (which refuses an
// endpointOverride the operator does not allow and a key that is not a
// service_account) and lists one page of the project's buckets. Its errors
// are not server.ErrInvalidServer, for the reason probeAWS gives. The project
// is the account of the store, and the location, when the store names one, its
// region.
func (p Prober) probeGCP(ctx context.Context, cs cloudStore, cc server.CloudConnection) (probe.CloudResult, error) {
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithSink(nil))
	g, err := p.Config.gcpClient(ctx, cs, cc)
	if err != nil {
		return probe.CloudResult{}, err
	}
	if err := p.Config.ProbeGCP(ctx, g, cs.gcp.Project); err != nil {
		return probe.CloudResult{}, err
	}
	d := map[string]string{DiscoveredEndpoint: bareEndpoint(gcsEndpoint(cs.gcp)), DiscoveredAccountID: cs.gcp.Project}
	if cs.gcp.Location != "" {
		d[DiscoveredRegion] = cs.gcp.Location
	}
	return probe.CloudResult{Discovered: d}, nil
}

func (p Prober) probe(ctx context.Context, spec s3v1alpha1.ObjectStoreConfig, conn server.Connection) (string, error) {
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

// storeConfigOf returns the S3 fields of a store kind of this operator, and its
// provider if it runs on a cloud.
func storeConfigOf(srv server.CloudServer) (s3v1alpha1.ObjectStoreConfig, cloudStore, error) {
	switch s := srv.(type) {
	case *s3v1alpha1.ClusterObjectStore:
		cs, err := clusterCloud(s)
		return s.Spec.ObjectStoreConfig, cs, err
	case *s3v1alpha1.ObjectStore:
		cs, err := namespacedCloud(s)
		return s.Spec.ObjectStoreConfig, cs, err
	}
	return s3v1alpha1.ObjectStoreConfig{}, cloudStore{}, fmt.Errorf("%w: %T", server.ErrUnknownServerKind, srv)
}
