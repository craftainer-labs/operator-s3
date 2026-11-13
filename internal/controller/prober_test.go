package controller

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/craftainer-labs/lib-operator-go/pkg/lifecycle"
	"github.com/craftainer-labs/lib-operator-go/pkg/server"

	s3v1alpha1 "github.com/craftainer-labs/operator-s3/api/v1alpha1"
	"github.com/craftainer-labs/operator-s3/internal/egress"
	"github.com/craftainer-labs/operator-s3/internal/s3client"
)

// versionStore is a Store that only answers the health probe.
type versionStore struct {
	Store
	version string
	err     error
	closed  int
}

func (v *versionStore) ServerVersion(context.Context) (string, error) { return v.version, v.err }
func (v *versionStore) CloseIdleConnections()                         { v.closed++ }

// probeFixture is a Prober whose NewStore hands out one versionStore and
// records the target it was built for.
func probeFixture(vs *versionStore) (Prober, *[]s3client.Target) {
	var targets []s3client.Target
	cfg := Config{
		Egress: egress.NewPolicy(egress.ModeStrict),
		NewStore: func(t s3client.Target) (Store, error) {
			targets = append(targets, t)
			return vs, nil
		},
	}
	return Prober{Config: cfg}, &targets
}

func probeConn() server.Connection {
	return server.Connection{
		Endpoint: "https://rustfs.example.test:9000",
		Admin:    map[string][]byte{AdminAccessKey: []byte("admin-ak"), AdminSecretKey: []byte("s3cret-sk")},
		CABundle: []byte("ca"), ServerName: "rustfs.internal", MinTLSVersion: 0x0304,
	}
}

// Both kinds are probed with the admin credentials, TLS settings, region and
// endpoint of the store.
func TestProberReadsTheVersionOfBothKinds(t *testing.T) {
	cluster := &s3v1alpha1.ClusterObjectStore{}
	cluster.Spec.Region = "eu-west-1"
	namespaced := &s3v1alpha1.ObjectStore{}

	for name, tc := range map[string]struct {
		srv    server.Server
		region string
	}{"cluster": {cluster, "eu-west-1"}, "namespaced": {namespaced, "us-east-1"}} {
		vs := &versionStore{version: "1.0.0-rc.5"}
		p, targets := probeFixture(vs)
		got, err := p.Probe(context.Background(), tc.srv, probeConn())
		if err != nil || got != "1.0.0-rc.5" {
			t.Fatalf("%s: %q, %v", name, got, err)
		}
		if len(*targets) != 1 {
			t.Fatalf("%s: %d clients", name, len(*targets))
		}
		tg := (*targets)[0]
		if tg.Endpoint != "https://rustfs.example.test:9000" || tg.Region != tc.region || tg.AccessKey != "admin-ak" ||
			tg.SecretKey != "s3cret-sk" || tg.ServerName != "rustfs.internal" || string(tg.CABundle) != "ca" ||
			tg.MinTLS != 0x0304 || tg.Dial == nil || tg.KeepAlive {
			t.Errorf("%s: target = %+v", name, tg)
		}
		if vs.closed != 1 {
			t.Errorf("%s: closed %d times, want 1", name, vs.closed)
		}
	}
}

// A store that answers but names no version is healthy with an empty one.
func TestProberToleratesAStoreWithoutAVersion(t *testing.T) {
	p, _ := probeFixture(&versionStore{err: s3client.ErrNoVersion})
	got, err := p.Probe(context.Background(), &s3v1alpha1.ObjectStore{}, probeConn())
	if err != nil || got != "" {
		t.Errorf("%q, %v", got, err)
	}
}

// Whatever fails, the error carries no credential.
func TestProberFailuresLeakNothing(t *testing.T) {
	srv := &s3v1alpha1.ClusterObjectStore{}
	ctx := context.Background()

	// A store that refuses the credentials.
	auth := errors.Join(lifecycle.ErrUnreachable, s3client.ErrConnAuth)
	p, _ := probeFixture(&versionStore{err: auth})
	_, err := p.Probe(ctx, srv, probeConn())
	if !errors.Is(err, s3client.ErrConnAuth) || strings.Contains(err.Error(), "s3cret-sk") || strings.Contains(err.Error(), "admin-ak") {
		t.Errorf("auth: %v", err)
	}

	// Missing credentials.
	conn := probeConn()
	delete(conn.Admin, AdminSecretKey)
	p, targets := probeFixture(&versionStore{})
	if _, err = p.Probe(ctx, srv, conn); !errors.Is(err, ErrAdminCredentials) || !errors.Is(err, server.ErrInvalidServer) || len(*targets) != 0 {
		t.Errorf("credentials: %v", err)
	}

	// A proxy the egress policy cannot dial through.
	conn = probeConn()
	conn.Proxy = &url.URL{Scheme: "ftp", Host: "proxy.example.test"}
	if _, err = p.Probe(ctx, srv, conn); !errors.Is(err, server.ErrInvalidServer) {
		t.Errorf("proxy: %v", err)
	}

	// A client that cannot be built.
	bad := Prober{Config: Config{
		Egress:   egress.NewPolicy(egress.ModeStrict),
		NewStore: func(s3client.Target) (Store, error) { return nil, s3client.ErrInvalidEndpoint },
	}}
	if _, err = bad.Probe(ctx, srv, probeConn()); !errors.Is(err, server.ErrInvalidServer) || !errors.Is(err, s3client.ErrInvalidEndpoint) {
		t.Errorf("client: %v", err)
	}

	// A store without Close (a fake) is still probed.
	plain := Prober{Config: Config{
		Egress:   egress.NewPolicy(egress.ModeStrict),
		NewStore: func(s3client.Target) (Store, error) { return &plainStore{}, nil },
	}}
	if got, err := plain.Probe(ctx, srv, probeConn()); err != nil || got != "v" {
		t.Errorf("plain: %q, %v", got, err)
	}

	// A store kind of another operator.
	if _, err = p.Probe(ctx, otherServer{}, probeConn()); !errors.Is(err, server.ErrUnknownServerKind) {
		t.Errorf("kind: %v", err)
	}
}

// plainStore has no CloseIdleConnections.
type plainStore struct{ Store }

func (plainStore) ServerVersion(context.Context) (string, error) { return "v", nil }

// otherServer is a server.Server this operator does not own.
type otherServer struct{ server.Server }
