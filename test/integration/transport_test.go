//go:build integration

package integration

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"strings"
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

// The health probe reads the server version from the admin API over the
// verified connection, and a failed one names a category, never the host or a
// key.
func TestServerVersion(t *testing.T) {
	sv := load(t)

	got, err := sv.client(t).ServerVersion(bg)
	if err != nil {
		t.Fatalf("ServerVersion: %v", err)
	}
	if got == "" || strings.Contains(got, "@") {
		t.Errorf("server version = %q, want the tag, short commit or package version", got)
	}
	t.Logf("RustFS reports version %q", got)

	// A wrong secret and an unknown access key: the store refuses, and the
	// error is an authentication failure that holds neither key nor host.
	for what, mutate := range map[string]func(*s3client.Target){
		"wrong secret":   func(tg *s3client.Target) { tg.SecretKey = "wrong-secret-0123456789" },
		"unknown access": func(tg *s3client.Target) { tg.AccessKey = "unknown-access-key" },
	} {
		_, err := sv.client(t, mutate).ServerVersion(bg)
		if !errors.Is(err, lifecycle.ErrUnreachable) || !errors.Is(err, s3client.ErrConnAuth) {
			t.Fatalf("%s: %v, want an authentication failure", what, err)
		}
		for _, secret := range []string{"wrong-secret-0123456789", "unknown-access-key", sv.access, sv.secret, sv.host} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("%s: the error leaks %q: %v", what, secret, err)
			}
		}
	}

	// The certificate is verified for the probe as for every other call.
	_, err = sv.client(t, func(tg *s3client.Target) { tg.CABundle = nil }).ServerVersion(bg)
	if !errors.Is(err, s3client.ErrConnTLS) || strings.Contains(err.Error(), sv.host) {
		t.Errorf("no CA bundle: %v", err)
	}
}
