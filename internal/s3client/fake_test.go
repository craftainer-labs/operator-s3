package s3client

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// fakeStore is a RustFS: the admin API and the S3 bucket API over an HTTPS
// test server, in memory. It checks the SigV4 signature of every request by
// signing the same request again, and records what it was asked. There is no
// real RustFS to run against until NO3b-2, so its routes follow the admin
// handlers of rustfs/rustfs at 1.0.0-rc.5.
type fakeStore struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	reqs     []string
	bucket   map[string]*fakeBucket
	user     map[string]*fakeUser
	policy   map[string]string
	key      map[string]*fakeKey
	quota    map[string]int64
	deleted  []string
	failOn   string
	statusOn map[string]int
	body     map[string]string
	// checksum and quotaType are what the last tagging and quota writes carried.
	checksum, quotaType string
	// special answers a request instead of the routes when it returns true.
	special func(w http.ResponseWriter, r *http.Request) bool
}

type fakeBucket struct {
	tags       map[string]string
	versioning string
	lifecycle  string
	lock       bool
	objects    bool
}

type fakeUser struct {
	status   string
	policies []string
}

type fakeKey struct{ parent, secret string }

const (
	testAccess = "admin-ak"
	testSecret = "admin-sk"
)

func newFakeStore(t *testing.T) *fakeStore {
	t.Helper()
	f := &fakeStore{
		t: t, bucket: map[string]*fakeBucket{}, user: map[string]*fakeUser{}, policy: map[string]string{},
		key: map[string]*fakeKey{}, quota: map[string]int64{}, statusOn: map[string]int{}, body: map[string]string{},
	}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// target is a Target for the fake that trusts its certificate and dials
// without an egress policy.
func (f *fakeStore) target() Target {
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw})
	return Target{
		Endpoint: f.srv.URL, AccessKey: testAccess, SecretKey: testSecret, CABundle: ca,
		Dial: (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
	}
}

func (f *fakeStore) client() *Client {
	f.t.Helper()
	c, err := New(f.target())
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

// requests returns what was asked, as "METHOD /path?query".
func (f *fakeStore) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reqs...)
}

func (f *fakeStore) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = nil
}

// asked reports whether a request line contains all of subs.
func (f *fakeStore) asked(subs ...string) bool {
	for _, r := range f.requests() {
		ok := true
		for _, s := range subs {
			ok = ok && strings.Contains(r, s)
		}
		if ok {
			return true
		}
	}
	return false
}

func (f *fakeStore) setBody(match, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.body[match] = body
}

func xmlError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, "<Error><Code>%s</Code><Message>secret detail</Message></Error>", code)
}

// verify signs the request again with the admin credentials and the signing
// time it carries, and compares the Authorization headers.
func (f *fakeStore) verify(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	_, rest, _ := strings.Cut(auth, "SignedHeaders=")
	signed, _, _ := strings.Cut(rest, ",")
	when, err := time.Parse("20060102T150405Z", r.Header.Get("X-Amz-Date"))
	if err != nil {
		return "no X-Amz-Date"
	}
	u := *r.URL
	u.Scheme, u.Host = "https", r.Host
	again := (&http.Request{Method: r.Method, URL: &u, Host: r.Host, Header: http.Header{}, ContentLength: r.ContentLength}).
		WithContext(context.Background())
	for _, name := range strings.Split(signed, ";") {
		if name != "host" && name != "content-length" {
			again.Header.Set(name, r.Header.Get(name))
		}
	}
	creds := aws.Credentials{AccessKeyID: testAccess, SecretAccessKey: testSecret}
	if err := v4.NewSigner().SignHTTP(context.Background(), creds, again, r.Header.Get("X-Amz-Content-Sha256"), "s3", "us-east-1", when); err != nil {
		return err.Error()
	}
	if again.Header.Get("Authorization") != auth {
		return "signature mismatch: " + auth + " != " + again.Header.Get("Authorization")
	}
	return ""
}

func (f *fakeStore) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	line := r.Method + " " + r.URL.Path
	if r.URL.RawQuery != "" {
		line += "?" + r.URL.RawQuery
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, line)
	if f.special != nil && f.special(w, r) {
		return
	}
	if bad := f.verify(r); bad != "" {
		f.t.Errorf("request %s: %s", line, bad)
		xmlError(w, http.StatusForbidden, "SignatureDoesNotMatch")
		return
	}
	for match, status := range f.statusOn {
		if strings.Contains(line, match) {
			if status/100 == 3 {
				w.Header().Set("Location", "https://elsewhere.example.test/")
			}
			xmlError(w, status, "Injected")
			return
		}
	}
	if f.failOn != "" && strings.Contains(line, f.failOn) {
		xmlError(w, http.StatusInternalServerError, "InternalError")
		return
	}
	for match, out := range f.body {
		if strings.Contains(line, match) {
			_, _ = io.WriteString(w, out)
			return
		}
	}
	if strings.HasPrefix(r.URL.Path, adminPrefix+"/") {
		f.admin(w, r, strings.TrimPrefix(r.URL.Path, adminPrefix), body)
		return
	}
	f.s3(w, r, strings.TrimPrefix(r.URL.Path, "/"), body)
}

func (f *fakeStore) admin(w http.ResponseWriter, r *http.Request, path string, body []byte) {
	q := r.URL.Query()
	switch r.Method + " " + path {
	case "GET /user-info":
		u := f.user[q.Get("accessKey")]
		if u == nil {
			xmlError(w, http.StatusNotFound, "XAdminNoSuchUser")
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": u.status, "policyName": strings.Join(u.policies, ",")})
	case "PUT /add-user":
		var in struct{ SecretKey, Status string }
		_ = json.Unmarshal(body, &in)
		f.user[q.Get("accessKey")] = &fakeUser{status: in.Status}
	case "PUT /set-user-status":
		f.user[q.Get("accessKey")].status = q.Get("status")
	case "DELETE /remove-user":
		if f.user[q.Get("accessKey")] == nil {
			xmlError(w, http.StatusNotFound, "XAdminNoSuchUser")
			return
		}
		delete(f.user, q.Get("accessKey"))
		f.deleted = append(f.deleted, "user:"+q.Get("accessKey"))
	case "PUT /add-canned-policy":
		f.policy[q.Get("name")] = string(body)
	case "DELETE /remove-canned-policy":
		if _, ok := f.policy[q.Get("name")]; !ok {
			xmlError(w, http.StatusNotFound, "XAdminNoSuchPolicy")
			return
		}
		delete(f.policy, q.Get("name"))
		f.deleted = append(f.deleted, "policy:"+q.Get("name"))
	case "POST /idp/builtin/policy/attach", "POST /idp/builtin/policy/detach":
		var in struct {
			Policies []string `json:"policies"`
			User     string   `json:"user"`
		}
		_ = json.Unmarshal(body, &in)
		u := f.user[in.User]
		if strings.HasSuffix(path, "attach") {
			u.policies = append(u.policies, in.Policies...)
			break
		}
		for _, p := range in.Policies {
			for i, have := range u.policies {
				if have == p {
					u.policies = append(u.policies[:i], u.policies[i+1:]...)
					break
				}
			}
		}
	case "GET /info-service-account":
		k := f.key[q.Get("accessKey")]
		if k == nil {
			xmlError(w, http.StatusNotFound, "XAdminNoSuchServiceAccount")
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"parentUser": k.parent, "accessKey": q.Get("accessKey")})
	case "PUT /add-service-accounts":
		var in struct{ TargetUser, AccessKey, SecretKey string }
		_ = json.Unmarshal(body, &in)
		f.key[in.AccessKey] = &fakeKey{parent: in.TargetUser, secret: in.SecretKey}
		_, _ = fmt.Fprintf(w, `{"credentials":{"accessKey":%q,"secretKey":%q}}`, in.AccessKey, in.SecretKey)
	case "POST /update-service-account":
		var in struct{ NewSecretKey string }
		_ = json.Unmarshal(body, &in)
		f.key[q.Get("accessKey")].secret = in.NewSecretKey
		w.WriteHeader(http.StatusNoContent)
	case "DELETE /delete-service-account":
		delete(f.key, q.Get("accessKey"))
		f.deleted = append(f.deleted, "key:"+q.Get("accessKey"))
	case "GET /list-service-accounts":
		accounts := []map[string]string{}
		for ak, k := range f.key {
			if k.parent == q.Get("user") {
				accounts = append(accounts, map[string]string{"accessKey": ak})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"accounts": accounts})
	case "GET /get-bucket-quota":
		_ = json.NewEncoder(w).Encode(map[string]any{"quota": f.quota[q.Get("bucket")], "size": 0})
	case "PUT /set-bucket-quota":
		var in struct {
			Quota     int64
			Quotatype string
		}
		_ = json.Unmarshal(body, &in)
		f.quota[q.Get("bucket")] = in.Quota
		f.quotaType = in.Quotatype
	default:
		if strings.HasPrefix(path, "/quota/") && r.Method == http.MethodDelete {
			delete(f.quota, strings.TrimPrefix(path, "/quota/"))
			return
		}
		xmlError(w, http.StatusNotImplemented, "NotImplemented")
	}
}

func (f *fakeStore) s3(w http.ResponseWriter, r *http.Request, name string, body []byte) {
	b := f.bucket[name]
	if r.Method == http.MethodPut && r.URL.RawQuery == "" {
		f.bucket[name] = &fakeBucket{tags: map[string]string{}, lock: r.Header.Get("X-Amz-Bucket-Object-Lock-Enabled") == "true"}
		return
	}
	if b == nil {
		xmlError(w, http.StatusNotFound, "NoSuchBucket")
		return
	}
	resource := ""
	for k := range r.URL.Query() {
		resource = k
	}
	switch r.Method + " " + resource {
	case "HEAD ":
	case "DELETE ":
		if b.objects {
			xmlError(w, http.StatusConflict, "BucketNotEmpty")
			return
		}
		delete(f.bucket, name)
		w.WriteHeader(http.StatusNoContent)
	case "GET tagging":
		if len(b.tags) == 0 {
			xmlError(w, http.StatusNotFound, "NoSuchTagSet")
			return
		}
		out := "<Tagging><TagSet>"
		for k, v := range b.tags {
			out += "<Tag><Key>" + k + "</Key><Value>" + v + "</Value></Tag>"
		}
		_, _ = io.WriteString(w, out+"</TagSet></Tagging>")
	case "PUT tagging":
		var in tagging
		if err := decodeXML(body, &in); err != nil {
			f.t.Errorf("tagging body: %v", err)
		}
		b.tags = map[string]string{}
		for _, t := range in.Tags {
			b.tags[t.Key] = t.Value
		}
		f.checksum = r.Header.Get("X-Amz-Checksum-Sha256")
	case "GET versioning":
		_, _ = io.WriteString(w, "<VersioningConfiguration><Status>"+b.versioning+"</Status></VersioningConfiguration>")
	case "PUT versioning":
		var in versioning
		if err := decodeXML(body, &in); err != nil {
			f.t.Errorf("versioning body: %v", err)
		}
		b.versioning = in.Status
	case "GET lifecycle":
		if b.lifecycle == "" {
			xmlError(w, http.StatusNotFound, "NoSuchLifecycleConfiguration")
			return
		}
		_, _ = io.WriteString(w, b.lifecycle)
	case "PUT lifecycle":
		b.lifecycle = string(body)
	case "DELETE lifecycle":
		b.lifecycle = ""
		w.WriteHeader(http.StatusNoContent)
	default:
		xmlError(w, http.StatusNotImplemented, "NotImplemented")
	}
}

// plainServer is an HTTP (not TLS) test server's address.
func plainServer(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	return srv.URL
}
