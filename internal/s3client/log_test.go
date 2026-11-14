package s3client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/craftainer-labs/lib-operator-go/pkg/lifecycle"
)

// logSink returns a context whose logger writes every level, V(1) included,
// to the returned buffer.
func logSink() (context.Context, *bytes.Buffer) {
	var buf bytes.Buffer
	return log.IntoContext(context.Background(), zap.New(zap.WriteTo(&buf), zap.UseDevMode(true))), &buf
}

// noLeak fails when the log holds anything a tenant or an attacker must not
// read from it: the host, a credential or the text of an answer.
func noLeak(t *testing.T, name, logged string, secrets ...string) {
	t.Helper()
	if logged == "" {
		t.Errorf("%s: nothing was logged", name)
	}
	for _, s := range append(secrets, testAccess, testSecret, "127.0.0.1", "10.1.2.3", "store.example.test", "secret detail") {
		if strings.Contains(logged, s) {
			t.Errorf("%s: the log holds %q: %s", name, s, logged)
		}
	}
}

var errRefused = errors.New("connection refused")

func isURLError(err error) bool {
	var u *url.Error
	return errors.As(err, &u)
}

// A failed claims-path call logs the operation and the category or the S3
// code, and nothing the cause or the answer says; the cause stays reachable.
func TestTheLogHoldsOnlyCategoriesAndCodes(t *testing.T) {
	f := newFakeStore(t)

	dialFails := func(c *Target) {
		c.Dial = func(context.Context, string, string) (net.Conn, error) {
			return nil, fmt.Errorf("dial tcp 10.1.2.3:9000: connect: %w", errRefused)
		}
	}
	tlsFails := func(c *Target) { c.CABundle = nil }
	for name, tc := range map[string]struct {
		mutate func(*Target)
		want   string
		cause  func(error) bool
	}{
		"connection refused": {dialFails, ErrConnFailed.Error(), func(err error) bool { return errors.Is(err, errRefused) }},
		"TLS failure":        {tlsFails, ErrConnTLS.Error(), isURLError},
	} {
		tg := f.target()
		tc.mutate(&tg)
		c, err := New(tg)
		if err != nil {
			t.Fatal(err)
		}
		ctx, buf := logSink()
		_, err = c.User(ctx, "alice-ak")
		if !errors.Is(err, lifecycle.ErrUnreachable) || !tc.cause(err) {
			t.Errorf("%s: %v", name, err)
		}
		noLeak(t, name, buf.String(), "alice-ak")
		if !strings.Contains(buf.String(), tc.want) || !strings.Contains(buf.String(), "GET /rustfs/admin/v3/user-info") {
			t.Errorf("%s: the log lacks the category and the operation: %s", name, buf)
		}
	}

	// The S3 path logs the same way.
	tg := f.target()
	tlsFails(&tg)
	c, err := New(tg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, buf := logSink()
	if _, err := c.Bucket(ctx, "bkt"); !errors.Is(err, ErrConnTLS) {
		t.Errorf("S3 TLS: %v", err)
	}
	noLeak(t, "S3 TLS", buf.String())
	if !strings.Contains(buf.String(), "HEAD bucket") {
		t.Errorf("S3 TLS: the log lacks the operation: %s", buf)
	}

	// An answer, with a body that names everything.
	leaky := "<Error><Code>AccessDenied</Code><Message>admin-ak admin-sk 127.0.0.1 secret detail</Message></Error>"
	for name, status := range map[string]int{"403": http.StatusForbidden, "503 with a body": http.StatusServiceUnavailable} {
		f.special = func(w http.ResponseWriter, _ *http.Request) bool {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(leaky))
			return true
		}
		ctx, buf := logSink()
		_, err := f.client().User(ctx, "alice-ak")
		var e *Error
		if !errors.As(err, &e) || e.Status != status || e.Code != "AccessDenied" {
			t.Errorf("%s: the cause is lost: %v", name, err)
		}
		if status == http.StatusForbidden && !errors.Is(err, ErrConnAuth) {
			t.Errorf("%s: %v", name, err)
		}
		noLeak(t, name, buf.String(), "alice-ak")
		if !strings.Contains(buf.String(), "AccessDenied") || !strings.Contains(buf.String(), "GET /rustfs/admin/v3/user-info") {
			t.Errorf("%s: the log lacks the code and the operation: %s", name, buf)
		}
	}

	// The same answer through minio-go.
	f.special = func(w http.ResponseWriter, _ *http.Request) bool {
		xmlError(w, http.StatusForbidden, "AccessDenied")
		return true
	}
	ctx, buf = logSink()
	if _, err := f.client().Bucket(ctx, "bkt"); !errors.Is(err, ErrConnAuth) {
		t.Errorf("S3 403: %v", err)
	}
	noLeak(t, "S3 403", buf.String())
}

// A code from the server is logged only when it is a plain token, and a quota
// path does not log its bucket.
func TestLoggableAndAdminOp(t *testing.T) {
	for in, want := range map[string]string{
		"AccessDenied": "AccessDenied", "a-b_9": "a-b_9", "": "", "has space": invalidCode, "10.1.2.3:9000": "invalid", "é": "invalid",
		strings.Repeat("a", 65): "invalid", strings.Repeat("a", 64): strings.Repeat("a", 64),
	} {
		if got := loggable(in); got != want {
			t.Errorf("loggable(%q) = %q", in, got)
		}
	}
	if adminOp(adminPrefix+"/quota/my-bucket") != adminPrefix+"/quota" || adminOp(adminPrefix+"/set-bucket-quota") != adminPrefix+"/set-bucket-quota" {
		t.Error("adminOp")
	}
}

// The quota path carries a bucket name; the request that fails is logged
// without it.
func TestAQuotaPathIsLoggedWithoutItsBucket(t *testing.T) {
	f := newFakeStore(t)
	f.special = func(w http.ResponseWriter, _ *http.Request) bool {
		w.WriteHeader(http.StatusInternalServerError)
		return true
	}
	ctx, buf := logSink()
	if err := f.client().setQuota(ctx, "tenant-private-bucket", 0); err == nil {
		t.Fatal("no error")
	}
	noLeak(t, "quota", buf.String(), "tenant-private-bucket")
	if !strings.Contains(buf.String(), "DELETE /rustfs/admin/v3/quota") {
		t.Errorf("the log lacks the operation: %s", buf)
	}
}
