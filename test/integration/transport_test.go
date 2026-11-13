//go:build integration

package integration

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"sync/atomic"
	"testing"

	"github.com/craftainer-labs/lib-operator-go/pkg/lifecycle"

	"github.com/craftainer-labs/operator-s3/internal/s3client"
)

// A Client built with KeepAlive reuses its connection for the admin API and
// the S3 API alike (the real server keeps connections open); one built
// without it dials for every request.
func TestKeepAliveAgainstARealServer(t *testing.T) {
	sv := load(t)
	const rounds = 10
	dials := func(keepAlive bool) int64 {
		var n atomic.Int64
		c := sv.client(t, func(tg *s3client.Target) {
			tg.KeepAlive = keepAlive
			dial := tg.Dial
			tg.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
				n.Add(1)
				return dial(ctx, network, addr)
			}
		})
		for range rounds {
			if _, err := c.OwnedPolicies(bg, "it-keepalive"); err != nil {
				t.Fatal(err)
			}
			if st, err := c.Bucket(bg, unique("it-absent")); err != nil || st.Exists {
				t.Fatalf("Bucket: %+v, %v", st, err)
			}
		}
		return n.Load()
	}
	if n := dials(true); n > 3 {
		t.Errorf("with keep-alive: %d connections for %d requests", n, 2*rounds)
	}
	if n := dials(false); n < 2*rounds {
		t.Errorf("without keep-alive: %d connections for %d requests", n, 2*rounds)
	}
}

// The certificate chain and the host name are verified: a store whose CA the
// client does not have, or whose name is not the certificate's, is a TLS
// failure with the fixed category; TLS 1.3 and the name override work.
func TestTLSIsVerified(t *testing.T) {
	sv := load(t)

	for what, mutate := range map[string]func(*s3client.Target){
		"no CA bundle": func(tg *s3client.Target) { tg.CABundle = nil },
		"another name": func(tg *s3client.Target) { tg.ServerName = "not-the-store.invalid" },
	} {
		_, err := sv.client(t, mutate).OwnedPolicies(bg, "it-tls")
		if !errors.Is(err, lifecycle.ErrUnreachable) || !errors.Is(err, s3client.ErrConnTLS) {
			t.Errorf("%s: %v, want a TLS failure", what, err)
		}
		if _, err := sv.client(t, mutate).Bucket(bg, unique("it-absent")); !errors.Is(err, s3client.ErrConnTLS) {
			t.Errorf("%s, S3 API: %v, want a TLS failure", what, err)
		}
	}

	// The certificate names localhost as well as the store's own host name, so
	// the name that is verified can differ from the one dialled.
	byName := sv.client(t, func(tg *s3client.Target) { tg.ServerName = "localhost" })
	if _, err := byName.OwnedPolicies(bg, "it-tls"); err != nil {
		t.Errorf("verifying localhost: %v", err)
	}
	modern := sv.client(t, func(tg *s3client.Target) { tg.MinTLS = tls.VersionTLS13 })
	if _, err := modern.OwnedPolicies(bg, "it-tls"); err != nil {
		t.Errorf("TLS 1.3: %v", err)
	}
}
