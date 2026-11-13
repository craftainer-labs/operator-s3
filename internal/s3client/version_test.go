package s3client

import (
	"errors"
	"strings"
	"testing"

	"github.com/craftainer-labs/lib-operator-go/pkg/lifecycle"
)

func TestServerVersion(t *testing.T) {
	f := newFakeStore(t)
	c := f.client()

	got, err := c.ServerVersion(bg)
	if err != nil || got != "1.0.0-rc.5" {
		t.Fatalf("ServerVersion = %q, %v", got, err)
	}
	if !f.asked("GET /rustfs/admin/v3/info") {
		t.Errorf("requests: %v", f.requests())
	}

	// No "@": the whole text is the version. The first server that names one
	// wins.
	for _, tc := range []struct{ body, want string }{
		{`{"info":{"servers":[{"version":" 1.2.3 "}]}}`, "1.2.3"},
		{`{"info":{"servers":[{"version":""},{"version":"d@v2"}]}}`, "v2"},
		{`{"info":{"servers":[{"version":"d@"},{"version":"1.0.0-rc.4"}]}}`, "1.0.0-rc.4"},
	} {
		f.setBody("GET /rustfs/admin/v3/info", tc.body)
		if got, err := c.ServerVersion(bg); err != nil || got != tc.want {
			t.Errorf("%s: %q, %v, want %q", tc.body, got, err, tc.want)
		}
	}
}

func TestServerVersionFailures(t *testing.T) {
	f := newFakeStore(t)
	c := f.client()

	// An answer without a version still proved the store is reachable, but it
	// is not a version.
	for _, body := range []string{`{}`, `{"info":{}}`, `{"info":{"servers":[]}}`, `{"info":{"servers":[{"version":" "}]}}`} {
		f.setBody("GET /rustfs/admin/v3/info", body)
		if _, err := c.ServerVersion(bg); !errors.Is(err, ErrNoVersion) {
			t.Errorf("%s: %v", body, err)
		}
	}
	f.setBody("GET /rustfs/admin/v3/info", "not json")
	if _, err := c.ServerVersion(bg); !errors.Is(err, ErrBadResponse) {
		t.Errorf("bad body: %v", err)
	}
	f.body = map[string]string{}

	// Wrong credentials: the store refuses the signature, and the error is an
	// authentication failure of an unreachable store with no key in it.
	f.lenient = true
	tg := f.target()
	tg.SecretKey = "wrong-secret-key"
	bad, err := New(tg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = bad.ServerVersion(bg)
	if !errors.Is(err, lifecycle.ErrUnreachable) || !errors.Is(err, ErrConnAuth) {
		t.Fatalf("wrong credentials: %v", err)
	}
	for _, secret := range []string{"wrong-secret-key", testAccess, testSecret, strings.TrimPrefix(f.srv.URL, "https://")} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("the error leaks %q: %v", secret, err)
		}
	}
	f.lenient = false

	f.failOn = "/info"
	if _, err := c.ServerVersion(bg); err == nil || errors.Is(err, ErrNoVersion) {
		t.Errorf("a failing store: %v", err)
	}
}
