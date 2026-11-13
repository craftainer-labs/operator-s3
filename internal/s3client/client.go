// Package s3client is the object store backend client (NR4). It talks to
// RustFS's plain-JSON admin API under /rustfs/admin/v3 for users, policies,
// service accounts and bucket quotas, signed with minio-go's SigV4 signer, and
// to its S3 API for buckets through minio-go/v7. MinIO's /minio/admin surface
// (encrypted payloads, AGPL madmin-go) is not used.
//
// A Client verifies the store's certificate chain and host name, never
// follows a redirect, and dials only through the egress policy it is given.
// Whatever the store answers reaches a condition or an Event only as one of
// the fixed texts below; the raw error goes to the operator log.
package s3client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/signer"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/craftainer-labs/lib-operator-go/pkg/lifecycle"

	"github.com/craftainer-labs/operator-s3/internal/egress"
)

var (
	// ErrInvalidEndpoint means the store endpoint is not an https URL with a
	// host, or carries credentials, a query or a fragment (user decision 1,
	// 2026-11-03: credentials come only from Secret references).
	ErrInvalidEndpoint = errors.New("the store endpoint must be an https URL without credentials")
	// ErrInvalidCA means the store's CA bundle holds no certificate.
	ErrInvalidCA = errors.New("the CA bundle holds no PEM certificate")
	// ErrNoDialer means a Target has no Dial: a Client never falls back to a
	// dialer that skips the egress policy.
	ErrNoDialer = errors.New("a dialer is required")
	// ErrNoCredentials means the admin access key or secret key is empty.
	ErrNoCredentials = errors.New("the admin credentials are empty")
	// ErrNotFound is matched by an *Error whose status is 404.
	ErrNotFound = errors.New("not found")
	// ErrBucketNotEmpty means a bucket still holds objects; the operator never
	// deletes objects.
	ErrBucketNotEmpty = errors.New("the bucket is not empty")
	// ErrInvalidTag means the bucket's tags, with the ownership marker, are not
	// valid S3 tags.
	ErrInvalidTag = errors.New("the bucket tags are not valid")
	// ErrBadResponse means the store answered 2xx with a body that does not
	// parse.
	ErrBadResponse = errors.New("the backend answered with an unexpected body")
)

// The categories of a failed request. A condition and an Event carry the
// error text to the tenant, and a tenant can aim a namespaced ObjectStore at
// any host, so they carry only one of these fixed texts: never an address, a
// resolved name or the raw error, which would turn the operator into a port
// scanner and a resolver. The raw error goes to the operator log.
var (
	ErrConnPolicy  = errors.New("connection refused by the egress policy")
	ErrConnTLS     = errors.New("TLS handshake failed")
	ErrConnAuth    = errors.New("authentication failed")
	ErrConnTimeout = errors.New("connection timed out")
	ErrConnFailed  = errors.New("connection failed")
)

// connError is lifecycle.ErrUnreachable with a category as its text. It still
// unwraps to the cause, for errors.Is, but Error never shows it.
type connError struct{ category, cause error }

func (e *connError) Error() string {
	return lifecycle.ErrUnreachable.Error() + ": " + e.category.Error()
}

func (e *connError) Unwrap() []error {
	return []error{lifecycle.ErrUnreachable, e.category, e.cause}
}

// Error is a non-2xx answer of the store. Its text is the status alone: the
// code and body come from a server a tenant may have chosen, so they never
// reach a condition.
type Error struct {
	Status int
	// Code is the S3 error code of the body, for the client's own decisions.
	Code string
}

func (e *Error) Error() string {
	return fmt.Sprintf("the backend answered HTTP %d", e.Status)
}

// Is makes errors.Is(err, ErrNotFound) true for a 404.
func (e *Error) Is(target error) bool { return target == ErrNotFound && e.Status == http.StatusNotFound }

// tlsText reports whether the text of a connection error is one of
// crypto/tls's failures ("tls: ...", "x509: ..."), not just a host name that
// contains "tls".
func tlsText(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "tls: ") || strings.Contains(msg, "x509: ")
}

// classify maps a transport failure to its category.
func classify(err error) error {
	var verify *tls.CertificateVerificationError
	var netErr net.Error
	switch {
	case errors.Is(err, egress.ErrBlocked):
		return ErrConnPolicy
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()):
		return ErrConnTimeout
	case errors.As(err, &verify) || tlsText(err.Error()):
		return ErrConnTLS
	}
	return ErrConnFailed
}

// Target is one store: where to go, who to be and how to trust it.
type Target struct {
	// Endpoint is the https URL of the store; the admin API is under it.
	Endpoint string
	// Region is the signing region; empty is us-east-1.
	Region string
	// AccessKey and SecretKey are the admin credentials.
	AccessKey, SecretKey string
	// ServerName overrides the host used for certificate verification.
	ServerName string
	// CABundle is PEM; empty uses the system roots.
	CABundle []byte
	// MinTLS is a crypto/tls version; below TLS 1.2 is raised to 1.2.
	MinTLS uint16
	// Dial opens the TCP connection (see package egress).
	Dial egress.DialFunc
}

// signFunc signs a request; signer.SignV4 in production.
type signFunc func(req http.Request, accessKey, secretKey, sessionToken, location string) *http.Request

// Client is one store. It is safe for concurrent use.
type Client struct {
	base   *url.URL
	hc     *http.Client
	mc     *minio.Client
	access string
	secret string
	region string
	sign   signFunc
}

// newMinio builds the S3 client; a variable so a test can make it fail.
var newMinio = minio.New

// maxBody bounds what is read of any answer.
const maxBody = 1 << 20

// New validates the target and builds its Client. It makes no request. The
// endpoint has no path: both APIs are served at its root.
func New(t Target) (*Client, error) {
	u, err := url.Parse(t.Endpoint)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		strings.Trim(u.Path, "/") != "" {
		// err is dropped: a url.Error would echo the URL and its userinfo.
		return nil, ErrInvalidEndpoint
	}
	if t.Dial == nil {
		return nil, ErrNoDialer
	}
	if t.AccessKey == "" || t.SecretKey == "" {
		return nil, ErrNoCredentials
	}
	cfg := &tls.Config{MinVersion: max(t.MinTLS, tls.VersionTLS12), ServerName: t.ServerName}
	if len(t.CABundle) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(t.CABundle) {
			return nil, ErrInvalidCA
		}
		cfg.RootCAs = pool
	}
	tr := &http.Transport{
		DialContext:           t.Dial,
		TLSClientConfig:       cfg,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		// A Client lives for one reconcile; idle connections would leak.
		DisableKeepAlives: true,
	}
	hc := &http.Client{
		Transport: tr,
		Timeout:   time.Minute,
		// A redirect would carry the request, signed, to a host the egress
		// policy has not seen named.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	region := t.Region
	if region == "" {
		region = "us-east-1"
	}
	u.Path = ""
	// MaxRetries 1 turns minio-go's retries off: the controller retries with
	// backoff. Path-style addressing, because the store has no wildcard DNS.
	mc, err := newMinio(u.Host, &minio.Options{
		Creds: credentials.NewStaticV4(t.AccessKey, t.SecretKey, ""), Secure: true, Transport: tr, Region: region,
		BucketLookup: minio.BucketLookupPath, MaxRetries: 1,
	})
	if err != nil {
		return nil, ErrInvalidEndpoint
	}
	return &Client{
		base: u, hc: hc, mc: mc, access: t.AccessKey, secret: t.SecretKey, region: region, sign: signer.SignV4,
	}, nil
}

// call is one request. path starts with "/" and is relative to the endpoint.
type call struct {
	method, path string
	query        url.Values
	body         []byte
	contentType  string
}

// do sends the admin request, signed, and returns the body of a 2xx answer.
func (c *Client) do(ctx context.Context, rq call) ([]byte, error) {
	u := *c.base
	u.Path = rq.path
	u.RawQuery = rq.query.Encode()
	var body io.ReadCloser = http.NoBody
	if len(rq.body) > 0 {
		body = io.NopCloser(bytes.NewReader(rq.body))
	}
	req := (&http.Request{
		Method: rq.method, URL: &u, Host: u.Host, Header: http.Header{}, Body: body, ContentLength: int64(len(rq.body)),
	}).WithContext(ctx)
	if rq.contentType != "" {
		req.Header.Set("Content-Type", rq.contentType)
	}
	sum := sha256.Sum256(rq.body)
	req.Header.Set("X-Amz-Content-Sha256", hex.EncodeToString(sum[:]))
	req = c.sign(*req, c.access, c.secret, "", c.region)
	status, data, err := c.exchange(req)
	if err != nil {
		return nil, c.unreachable(ctx, rq.method+" "+rq.path, err)
	}
	if status/100 == 2 {
		return data, nil
	}
	return nil, c.refused(ctx, rq.method+" "+rq.path, status, errorCode(data), snippet(data))
}

// unreachable logs a transport failure and returns its fixed category.
func (c *Client) unreachable(ctx context.Context, op string, err error) error {
	log.FromContext(ctx).Error(err, "cannot reach the backend", "request", op)
	return &connError{category: classify(err), cause: err}
}

// refused logs a non-2xx answer and returns it as an *Error; 401 and 403 are
// an authentication failure of an unreachable store.
func (c *Client) refused(ctx context.Context, op string, status int, code, detail string) error {
	e := &Error{Status: status, Code: code}
	if status == http.StatusNotFound {
		log.FromContext(ctx).V(1).Info("the backend found nothing", "request", op, "code", code)
	} else {
		log.FromContext(ctx).Error(e, "the backend refused the request", "request", op, "code", code, "detail", detail)
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return &connError{category: ErrConnAuth, cause: e}
	}
	return e
}

// s3 turns the error of a minio-go call into the errors of this package.
func (c *Client) s3(ctx context.Context, op string, err error) error {
	if err == nil {
		return nil
	}
	var resp minio.ErrorResponse
	if errors.As(err, &resp) {
		return c.refused(ctx, op, resp.StatusCode, resp.Code, resp.Message)
	}
	return c.unreachable(ctx, op, err)
}

func (c *Client) exchange(req *http.Request) (int, []byte, error) {
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	return resp.StatusCode, data, err
}

// errorCode returns the <Code> of an S3 error body, or "".
func errorCode(data []byte) string {
	var e struct {
		Code string `xml:"Code"`
	}
	_ = xml.Unmarshal(data, &e) // a body that is not XML has no code
	return e.Code
}

// snippet is the start of a body, for the operator log.
func snippet(data []byte) string {
	return string(data[:min(len(data), 512)])
}
