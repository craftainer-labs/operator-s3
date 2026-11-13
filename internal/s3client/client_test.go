package s3client

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/craftainer-labs/lib-operator-go/pkg/lifecycle"

	"github.com/craftainer-labs/operator-s3/internal/egress"
)

func plainDial(ctx context.Context, network, addr string) (net.Conn, error) {
	return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, addr)
}

func TestNewRejectsWhatItCannotUse(t *testing.T) {
	good := Target{Endpoint: "https://store.example.test:9000", AccessKey: "a", SecretKey: "s", Dial: plainDial}
	for _, endpoint := range []string{
		"", "http://store.example.test", "https://", "://x", "https://user:s3cret@store.example.test",
		"https://:s3cret@store.example.test", "https://store.example.test?x=1", "https://store.example.test#f", "ftp://store",
	} {
		tg := good
		tg.Endpoint = endpoint
		_, err := New(tg)
		if !errors.Is(err, ErrInvalidEndpoint) {
			t.Errorf("endpoint %q: %v", endpoint, err)
		}
		if err != nil && strings.Contains(err.Error(), "s3cret") {
			t.Errorf("endpoint %q: the error echoes the credentials: %v", endpoint, err)
		}
	}
	tg := good
	tg.Dial = nil
	if _, err := New(tg); !errors.Is(err, ErrNoDialer) {
		t.Errorf("no dialer: %v", err)
	}
	for _, empty := range []func(*Target){func(t *Target) { t.AccessKey = "" }, func(t *Target) { t.SecretKey = "" }} {
		tg = good
		empty(&tg)
		if _, err := New(tg); !errors.Is(err, ErrNoCredentials) {
			t.Errorf("empty credentials: %v", err)
		}
	}
	tg = good
	tg.CABundle = []byte("not a certificate")
	if _, err := New(tg); !errors.Is(err, ErrInvalidCA) {
		t.Errorf("bad CA: %v", err)
	}
}

func TestNewDefaults(t *testing.T) {
	tg := Target{Endpoint: "https://store.example.test:9000/prefix/", AccessKey: "a", SecretKey: "s", Dial: plainDial}
	c, err := New(tg)
	if err != nil {
		t.Fatal(err)
	}
	if c.region != "us-east-1" || c.base.Path != "/prefix" {
		t.Errorf("region = %q, path = %q", c.region, c.base.Path)
	}
	tg.Region = "eu-west-1"
	if c, _ = New(tg); c.region != "eu-west-1" {
		t.Errorf("region = %q", c.region)
	}
	cfg := c.hc.Transport.(*http.Transport).TLSClientConfig
	if cfg.MinVersion != tls.VersionTLS12 || cfg.InsecureSkipVerify {
		t.Errorf("tls = %+v", cfg)
	}
	tg.MinTLS = tls.VersionTLS13
	c, _ = New(tg)
	if c.hc.Transport.(*http.Transport).TLSClientConfig.MinVersion != tls.VersionTLS13 {
		t.Error("TLS 1.3 was not kept")
	}
}

// Every request is signed with SigV4 for the s3 service and the store's
// region, and the fake checks the signature by signing the request again. A
// request outside the endpoint's path prefix would fail the same check.
func TestRequestsAreSigned(t *testing.T) {
	f := newFakeStore(t)
	tg := f.target()
	tg.Region = "us-east-1"
	c, err := New(tg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Bucket(context.Background(), "b1"); err != nil {
		t.Fatal(err)
	}
	if err := c.CreateBucket(context.Background(), BucketParams{Name: "b1", Marker: "ns/b1/uid", Versioning: true}); err != nil {
		t.Fatal(err)
	}
	if !f.asked("PUT /b1?versioning=") {
		t.Errorf("requests: %v", f.requests())
	}
}

func TestSigningFailureIsReturned(t *testing.T) {
	f := newFakeStore(t)
	c := f.client()
	boom := errors.New("cannot sign")
	c.sign = func(context.Context, aws.Credentials, *http.Request, string, string, string, time.Time) error { return boom }
	if _, err := c.do(context.Background(), call{method: http.MethodGet, path: "/b"}); !errors.Is(err, boom) {
		t.Errorf("do: %v", err)
	}
	if len(f.requests()) != 0 {
		t.Error("an unsigned request was sent")
	}
}

// What the store says never reaches a condition: an HTTP error is its status
// alone, 401 and 403 are an unreachable server with a fixed category.
func TestAnswersBecomeFixedErrors(t *testing.T) {
	f := newFakeStore(t)
	c := f.client()
	ctx := context.Background()
	get := call{method: http.MethodGet, path: "/b"}

	f.statusOn["GET /b"] = http.StatusInternalServerError
	_, err := c.do(ctx, get)
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Status != 500 || apiErr.Code != "Injected" {
		t.Fatalf("500: %v", err)
	}
	if err.Error() != "the backend answered HTTP 500" || errors.Is(err, ErrNotFound) {
		t.Errorf("500 text = %q", err)
	}

	f.statusOn["GET /b"] = http.StatusNotFound
	if _, err := c.do(ctx, get); !errors.Is(err, ErrNotFound) {
		t.Errorf("404: %v", err)
	}

	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		f.statusOn["GET /b"] = status
		_, err := c.do(ctx, get)
		if !errors.Is(err, lifecycle.ErrUnreachable) || !errors.Is(err, ErrConnAuth) {
			t.Errorf("%d: %v", status, err)
		}
		if err.Error() != "server unreachable: authentication failed" {
			t.Errorf("%d text = %q", status, err)
		}
	}

	// A redirect is not followed: it is the answer.
	f.statusOn["GET /b"] = http.StatusTemporaryRedirect
	if _, err := c.do(ctx, get); !errors.As(err, &apiErr) || apiErr.Status != http.StatusTemporaryRedirect {
		t.Errorf("redirect: %v", err)
	}
	if f.asked("elsewhere") {
		t.Error("the redirect was followed")
	}

	// A body that is not XML has no code.
	delete(f.statusOn, "GET /b")
	f.statusOn["GET /c"] = http.StatusBadGateway
	if _, err := c.do(ctx, call{method: http.MethodGet, path: "/c"}); !errors.As(err, &apiErr) || apiErr.Code != "Injected" {
		t.Errorf("502: %v", err)
	}
	if errorCode([]byte("<html>")) != "" || errorCode(nil) != "" {
		t.Error("a body without a code has one")
	}
}

func TestSnippetIsBounded(t *testing.T) {
	if got := snippet([]byte(strings.Repeat("x", 600))); len(got) != 512 {
		t.Errorf("len = %d", len(got))
	}
	if snippet([]byte("short")) != "short" {
		t.Error("short body changed")
	}
}

// A connection that fails is an unreachable server with a category, and the
// text never carries the address, the resolved name or the raw error.
func TestTransportFailuresAreCategorised(t *testing.T) {
	f := newFakeStore(t)
	ctx := context.Background()
	dials := map[string]struct {
		dial egress.DialFunc
		want error
	}{
		"policy": {func(context.Context, string, string) (net.Conn, error) {
			return nil, fmt.Errorf("%w: 10.1.2.3", egress.ErrBlocked)
		}, ErrConnPolicy},
		"timeout": {func(context.Context, string, string) (net.Conn, error) {
			return nil, fmt.Errorf("dial 10.1.2.3:9000: %w", context.DeadlineExceeded)
		}, ErrConnTimeout},
		"failed": {func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("dial tcp 10.1.2.3:9000: connect: connection refused")
		}, ErrConnFailed},
	}
	for name, tc := range dials {
		tg := f.target()
		tg.Dial = tc.dial
		c, err := New(tg)
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.Bucket(ctx, "b")
		if !errors.Is(err, lifecycle.ErrUnreachable) || !errors.Is(err, tc.want) {
			t.Errorf("%s: %v", name, err)
		}
		if strings.Contains(err.Error(), "10.1.2.3") || err.Error() != "server unreachable: "+tc.want.Error() {
			t.Errorf("%s: the text %q is not the fixed one", name, err)
		}
	}

	// A certificate the CA bundle does not vouch for.
	tg := f.target()
	tg.CABundle = nil
	c, _ := New(tg)
	if _, err := c.Bucket(ctx, "b"); !errors.Is(err, ErrConnTLS) {
		t.Errorf("unknown authority: %v", err)
	}
	// A certificate for another name: serverName decides.
	tg = f.target()
	tg.ServerName = "other.example.test"
	c, _ = New(tg)
	if _, err := c.Bucket(ctx, "b"); !errors.Is(err, ErrConnTLS) {
		t.Errorf("wrong server name: %v", err)
	}
	// Not TLS at all.
	tg = f.target()
	tg.Endpoint = "https://" + strings.TrimPrefix(plainServer(t), "http://")
	c, _ = New(tg)
	if _, err := c.Bucket(ctx, "b"); !errors.Is(err, ErrConnTLS) {
		t.Errorf("plain server: %v", err)
	}
}

func TestClassify(t *testing.T) {
	if classify(errors.New("tls: handshake failure")) != ErrConnTLS || classify(errors.New("x509: certificate expired")) != ErrConnTLS {
		t.Error("TLS texts")
	}
	if classify(errors.New("lookup tls-host.example.test: no such host")) != ErrConnFailed {
		t.Error("a host name containing tls is not a TLS failure")
	}
}

// The client timeout is a timeout, and a body cut short is a failed
// connection.
func TestTimeoutsAndTruncatedBodies(t *testing.T) {
	f := newFakeStore(t)
	c := f.client()
	c.hc.Timeout = 50 * time.Millisecond
	f.special = func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/slow":
			time.Sleep(300 * time.Millisecond)
		case "/cut":
			w.Header().Set("Content-Length", "100")
			_, _ = w.Write([]byte("short"))
			conn, _, _ := w.(http.Hijacker).Hijack()
			_ = conn.Close()
		default:
			return false
		}
		return true
	}
	if _, err := c.do(context.Background(), call{method: http.MethodGet, path: "/slow"}); !errors.Is(err, ErrConnTimeout) {
		t.Errorf("slow: %v", err)
	}
	c.hc.Timeout = time.Minute
	if _, err := c.do(context.Background(), call{method: http.MethodGet, path: "/cut"}); !errors.Is(err, lifecycle.ErrUnreachable) {
		t.Errorf("cut: %v", err)
	}
}
