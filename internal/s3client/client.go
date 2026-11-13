// Package s3client is the object store backend client (NR4). It talks to
// RustFS's plain-JSON admin API under /rustfs/admin/v3 for users, policies,
// service accounts and bucket quotas, and to its S3 API for buckets, with
// every request signed with AWS Signature Version 4. MinIO's /minio/admin
// surface (encrypted payloads, AGPL client library) is not used.
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

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
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

// signFunc signs a request; v4.Signer.SignHTTP in production. A variable so a
// test can make signing fail.
type signFunc func(ctx context.Context, creds aws.Credentials, r *http.Request, payloadHash, service, region string, t time.Time) error

// Client is one store. It is safe for concurrent use.
type Client struct {
	base   *url.URL
	hc     *http.Client
	creds  aws.Credentials
	region string
	now    func() time.Time
	sign   signFunc
}

// maxBody bounds what is read of any answer.
const maxBody = 1 << 20

// New validates the target and builds its Client. It makes no request.
func New(t Target) (*Client, error) {
	u, err := url.Parse(t.Endpoint)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
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
	u.Path = strings.TrimRight(u.Path, "/")
	signer := v4.NewSigner()
	return &Client{
		base: u, hc: hc, region: region, now: time.Now,
		creds: aws.Credentials{AccessKeyID: t.AccessKey, SecretAccessKey: t.SecretKey},
		sign: func(ctx context.Context, creds aws.Credentials, r *http.Request, hash, service, region string, at time.Time) error {
			return signer.SignHTTP(ctx, creds, r, hash, service, region, at)
		},
	}, nil
}

// call is one request. path starts with "/" and is relative to the endpoint.
type call struct {
	method, path string
	query        url.Values
	body         []byte
	contentType  string
	header       map[string]string
}

// do sends the request, signed, and returns the body of a 2xx answer.
func (c *Client) do(ctx context.Context, rq call) ([]byte, error) {
	u := *c.base
	u.Path += rq.path
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
	for k, v := range rq.header {
		req.Header.Set(k, v)
	}
	sum := sha256.Sum256(rq.body)
	hash := hex.EncodeToString(sum[:])
	req.Header.Set("X-Amz-Content-Sha256", hash)
	if err := c.sign(ctx, c.creds, req, hash, "s3", c.region, c.now()); err != nil {
		return nil, err
	}
	status, data, err := c.exchange(req)
	if err != nil {
		log.FromContext(ctx).Error(err, "cannot reach the backend", "method", rq.method, "path", rq.path)
		return nil, &connError{category: classify(err), cause: err}
	}
	if status/100 == 2 {
		return data, nil
	}
	e := &Error{Status: status, Code: errorCode(data)}
	if status == http.StatusNotFound {
		log.FromContext(ctx).V(1).Info("the backend found nothing", "method", rq.method, "path", rq.path, "code", e.Code)
	} else {
		log.FromContext(ctx).Error(e, "the backend refused the request", "method", rq.method, "path", rq.path,
			"code", e.Code, "body", snippet(data))
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return nil, &connError{category: ErrConnAuth, cause: e}
	}
	return nil, e
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
