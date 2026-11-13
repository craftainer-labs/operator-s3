//go:build integration

// Package integration runs the operator's RustFS client against a real
// RustFS with TLS on (see README.md here): the same s3client.Client the
// controllers use, so what the fake in the unit tests only claims is checked
// against the server. The admin routes, the SigV4 signatures, the answers for
// missing things, the bucket configuration XML, the quota body, the policies
// and what they confine are all asked of the real thing.
package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/signer"

	"github.com/craftainer-labs/operator-s3/internal/s3client"
)

const (
	// adminPrefix is the RustFS admin API, as in the client.
	adminPrefix = "/rustfs/admin/v3"
	// region is the one RustFS signs for unless it is configured otherwise.
	region = "us-east-1"
)

var bg = context.Background()

// server is the RustFS under test, from the environment.
type server struct {
	endpoint       string
	host           string
	ca             []byte
	access, secret string
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// readFile reads a file named by the test environment through an os.Root on
// its directory, so the read cannot leave that directory.
func readFile(t *testing.T, path string) []byte {
	t.Helper()
	dir, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	b, err := dir.ReadFile(filepath.Base(path))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// setting is the value of an environment variable, or the content of the file
// that KEY_FILE names. A missing one fails the test: running the tag without
// a server is a mistake, not a reason to skip.
func setting(t *testing.T, key string) string {
	t.Helper()
	if v := os.Getenv(key); v != "" {
		return v
	}
	file := os.Getenv(key + "_FILE")
	if file == "" {
		t.Fatalf("%s or %s_FILE is not set; see test/integration/README.md", key, key)
	}
	return strings.TrimSpace(string(readFile(t, file)))
}

// load reads the connection settings.
func load(t *testing.T) server {
	t.Helper()
	endpoint := strings.TrimRight(env("S3_ENDPOINT", "https://localhost:9000"), "/")
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		t.Fatalf("S3_ENDPOINT %q is not an https URL", endpoint)
	}
	caFile := os.Getenv("S3_CA_FILE")
	if caFile == "" {
		t.Fatal("S3_CA_FILE is not set; see test/integration/README.md")
	}
	s := server{
		endpoint: endpoint, host: u.Host, ca: readFile(t, caFile),
		access: setting(t, "S3_ADMIN_ACCESS_KEY"), secret: setting(t, "S3_ADMIN_SECRET_KEY"),
	}
	s.waitReady(t)
	return s
}

// target is a verified connection as the admin.
func (s server) target() s3client.Target {
	return s3client.Target{
		Endpoint: s.endpoint, AccessKey: s.access, SecretKey: s.secret, CABundle: s.ca, MinTLS: tls.VersionTLS12,
		// Direct dial: the egress policy refuses loopback and is tested on
		// its own; this lane tests the server, its answers and the TLS
		// handshake.
		Dial: (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
	}
}

// client is the Client the controllers build for the store.
func (s server) client(t *testing.T, mutate ...func(*s3client.Target)) *s3client.Client {
	t.Helper()
	tg := s.target()
	for _, m := range mutate {
		m(&tg)
	}
	c, err := s3client.New(tg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.CloseIdleConnections)
	return c
}

// transport trusts the test CA.
func (s server) transport(t *testing.T) *http.Transport {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(s.ca) {
		t.Fatal("S3_CA_FILE holds no PEM certificate")
	}
	tr := &http.Transport{
		TLSClientConfig:     &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: 10 * time.Second,
	}
	t.Cleanup(tr.CloseIdleConnections)
	return tr
}

// minio is an S3 client for the object operations, as the credentials given.
func (s server) minio(t *testing.T, access, secret string) *minio.Client {
	t.Helper()
	mc, err := minio.New(s.host, &minio.Options{
		Creds: credentials.NewStaticV4(access, secret, ""), Secure: true, Transport: s.transport(t), Region: region,
		BucketLookup: minio.BucketLookupPath, MaxRetries: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return mc
}

// admin is an S3 client as the admin.
func (s server) admin(t *testing.T) *minio.Client { return s.minio(t, s.access, s.secret) }

// raw sends one signed admin request as the admin and returns the status and
// the body: for what the Client's own errors do not show, such as the exact
// status of a missing thing.
func (s server) raw(t *testing.T, method, path string, query url.Values, body []byte) (int, []byte) {
	t.Helper()
	return s.rawAs(t, s.access, s.secret, method, path, query, body)
}

func (s server) rawAs(t *testing.T, access, secret, method, path string, query url.Values, body []byte) (int, []byte) {
	t.Helper()
	status, data, err := s.send(t, access, secret, method, path, query, body)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return status, data
}

func (s server) send(t *testing.T, access, secret, method, path string, query url.Values, body []byte) (int, []byte, error) {
	t.Helper()
	u := s.endpoint + adminPrefix + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(bg, method, u, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	sum := sha256.Sum256(body)
	req.Header.Set("X-Amz-Content-Sha256", hex.EncodeToString(sum[:]))
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	req = signer.SignV4(*req, access, secret, "", region)
	resp, err := (&http.Client{Transport: s.transport(t), Timeout: time.Minute}).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, data, err
}

// ready makes the first test wait until the server answers an authenticated
// admin request: it listens before its IAM is loaded, and answers 500 until
// then.
var (
	readyMu sync.Mutex
	ready   bool
)

func (s server) waitReady(t *testing.T) {
	t.Helper()
	readyMu.Lock()
	defer readyMu.Unlock()
	// Only a success is remembered: a test that gives up leaves the wait to the
	// next one.
	if ready {
		return
	}
	func() {
		deadline := time.Now().Add(2 * time.Minute)
		for {
			status, body, err := s.send(t, s.access, s.secret, http.MethodGet, "/list-canned-policies", nil, nil)
			if err == nil && status == http.StatusOK {
				ready = true
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("the server is not ready: HTTP %d, %v: %s", status, err, body)
			}
			time.Sleep(time.Second)
		}
	}()
}

// unique is a name no other test run uses, in the shape of a lower-case
// bucket name and an IAM user.
func unique(prefix string) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return prefix + "-" + hex.EncodeToString(b)
}

// newSecret is a 40-character secret key, as the controller makes them.
func newSecret() string { return rand.Text() + rand.Text()[:14] }

// eventually runs fn until it returns nil or the limit passes.
func eventually(t *testing.T, limit time.Duration, what string, fn func() error) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for {
		err := fn()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %v", what, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// dropAfter removes the bucket, with everything in it (all versions), when the
// test ends. The operator never does that; the tests clean up after
// themselves.
func (s server) dropAfter(t *testing.T, name string) {
	t.Helper()
	mc := s.admin(t)
	t.Cleanup(func() {
		for o := range mc.ListObjects(bg, name, minio.ListObjectsOptions{Recursive: true, WithVersions: true}) {
			if o.Err != nil {
				t.Logf("cleanup of %s: %v", name, o.Err)
				break
			}
			if err := mc.RemoveObject(bg, name, o.Key, minio.RemoveObjectOptions{VersionID: o.VersionID}); err != nil {
				t.Logf("cleanup of %s/%s: %v", name, o.Key, err)
			}
		}
		if err := mc.RemoveBucket(bg, name); err != nil {
			t.Logf("cleanup of %s: %v", name, err)
		}
	})
}

// makeBucket creates a bucket as the admin, outside the operator.
func (s server) makeBucket(t *testing.T, name string, lock bool) *minio.Client {
	t.Helper()
	mc := s.admin(t)
	if err := mc.MakeBucket(bg, name, minio.MakeBucketOptions{Region: region, ObjectLocking: lock}); err != nil {
		t.Fatalf("make bucket %s: %v", name, err)
	}
	s.dropAfter(t, name)
	return mc
}

// access is the IAM side of a BucketAccess for the bucket and prefix.
func access(user, bucket, prefix string, write bool) s3client.AccessParams {
	doc := s3client.AccessPolicy(bucket, prefix, write)
	return s3client.AccessParams{User: user, Secret: newSecret(), Policy: s3client.PolicyName(user, doc), PolicyDoc: doc}
}

// grant is what the BucketAccess controller does for a new claim: the user,
// its policy and the policy attached. The user (and with it its service
// accounts and policies) is removed when the test ends.
func grant(t *testing.T, c *s3client.Client, user, bucket, prefix string, write bool) s3client.AccessParams {
	t.Helper()
	p := access(user, bucket, prefix, write)
	cur, err := c.User(bg, user)
	if err != nil {
		t.Fatalf("User: %v", err)
	}
	if err := c.ApplyAccess(bg, p, cur); err != nil {
		t.Fatalf("ApplyAccess: %v", err)
	}
	t.Cleanup(func() {
		if err := c.DeleteAccess(bg, user, nil); err != nil {
			t.Logf("cleanup of %s: %v", user, err)
		}
	})
	return p
}
